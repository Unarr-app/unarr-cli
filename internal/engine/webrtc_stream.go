// Package engine — webrtc_stream.go implements the daemon side of the custom
// WebRTC byte-streaming protocol. The browser opens an RTCDataChannel via
// SDP exchange (signalled over the web's HTTP + SSE relay); this code:
//
//   1. Parses the browser's SDP offer.
//   2. Creates a pion PeerConnection bound to the configured ICE servers.
//   3. Answers + trickles its own ICE candidates back through the signal client.
//   4. On DataChannel open, sends a HELLO frame describing the file.
//   5. Services RangeReq frames by reading from disk and emitting RangeData
//      chunks (16 KiB each) followed by a RangeEnd.
//   6. Honours app-level backpressure via SetBufferedAmountLowThreshold +
//      OnBufferedAmountLow — Chromium closes a DataChannel when bufferedAmount
//      exceeds 16 MiB, so we MUST pause the writer.
//
// No anacrolix, no torrent metadata. Just a peer-to-peer file server over
// WebRTC. Pass-through path; transcoding lives in transcoder.go (Fase 2.5).

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/engine/wire"
)

// Tunables — values match the protocol spec in plan/clever-weaving-dove.md.
const (
	// dcChunkPayload is the per-frame application payload size. Must match
	// wire.MaxChunkPayload so RangeData frames fit one SCTP message.
	dcChunkPayload = wire.MaxChunkPayload
	// dcHighWatermark is the bufferedAmount cap above which the writer pauses.
	// Chromium closes DCs above 16 MiB; pause well below.
	dcHighWatermark = 8 << 20
	// dcLowWatermark triggers OnBufferedAmountLow → resume the writer.
	dcLowWatermark = 1 << 20
	// rangeReqConcurrency is the cap on in-flight range responses per session.
	rangeReqConcurrency = 4
	// helloDeadline is the max wait for the DataChannel to open after answer.
	helloDeadline = 30 * time.Second
)

// WebRTCStreamConfig describes a single browser ↔ daemon stream session.
type WebRTCStreamConfig struct {
	SessionID  string
	FilePath   string
	FileName   string
	FileSize   int64
	ICEServers []webrtc.ICEServer
	Signal     *agent.Client
	// Logger receives diagnostic events; a nil logger swallows everything.
	Logger StreamLogger
	// Transcode steers on-the-fly transcoding when source codecs are not
	// browser-decodable (HEVC/AV1/AC3/DTS). Empty FFmpegPath disables it.
	Transcode TranscodeRuntime
	// Quality overrides the cap from Transcode for this session. One of
	// "2160p" | "1080p" | "720p" | "480p" | "original" | "" (= defer to
	// Transcode defaults).
	Quality string
}

// TranscodeRuntime carries the resolved ffmpeg/ffprobe paths + tunables so
// each session can decide whether to passthrough or pipe through ffmpeg.
type TranscodeRuntime struct {
	FFmpegPath   string
	FFprobePath  string
	HWAccel      HWAccel
	Preset       string
	VideoBitrate string
	AudioBitrate string
	MaxHeight    int
	// Disabled forces passthrough for every file even when codecs are not
	// browser-friendly. Useful when the user explicitly turns transcoding
	// off in config.
	Disabled bool
}

// StreamLogger is an injectable logger so tests can capture events.
type StreamLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...any)  {}
func (nopLogger) Warnf(string, ...any)  {}
func (nopLogger) Errorf(string, ...any) {}

func logger(l StreamLogger) StreamLogger {
	if l == nil {
		return nopLogger{}
	}
	return l
}

// qualityCap maps a session's Quality label to a (MaxHeight, VideoBitrate)
// pair. An empty label or "original" returns zero-values, signalling "no
// override" to the caller.
type qualityCap struct {
	MaxHeight    int
	VideoBitrate string // ffmpeg -b:v string, e.g. "3500k"
}

func resolveQualityCap(label string) qualityCap {
	switch label {
	case "2160p":
		return qualityCap{MaxHeight: 2160, VideoBitrate: "25000k"}
	case "1080p":
		return qualityCap{MaxHeight: 1080, VideoBitrate: "6000k"}
	case "720p":
		return qualityCap{MaxHeight: 720, VideoBitrate: "3500k"}
	case "480p":
		return qualityCap{MaxHeight: 480, VideoBitrate: "1500k"}
	default:
		// "original", "auto", "" → defer to config.
		return qualityCap{}
	}
}

// buildStreamSource picks between passthrough and transcoded source. ffprobe
// failure or missing ffmpeg falls back to passthrough — the browser surfaces
// a clearer codec error than us refusing to start.
//
// Quality override (cfg.Quality) can force a downscale even when the source
// codec is browser-friendly: a 4K h264 file watched on a phone with quality
// "720p" must transcode (otherwise we'd ship 4K bytes for a 6" screen).
func buildStreamSource(
	ctx context.Context,
	abs string,
	displayName string,
	cfg WebRTCStreamConfig,
	log StreamLogger,
) (streamSource, error) {
	tc := cfg.Transcode
	qcap := resolveQualityCap(cfg.Quality)

	if tc.Disabled || tc.FFmpegPath == "" || tc.FFprobePath == "" {
		return newDiskFileSource(abs)
	}

	probe, err := ProbeFile(ctx, tc.FFprobePath, abs)
	if err != nil {
		log.Warnf("[wrtc %s] probe failed (%v) — passthrough", agent.ShortID(cfg.SessionID), err)
		return newDiskFileSource(abs)
	}
	action := DecideAction(probe)

	// Quality cap can promote a passthrough/remux decision into a full video
	// transcode when the source resolution exceeds the requested cap.
	if qcap.MaxHeight > 0 && probe.Height > 0 && probe.Height > qcap.MaxHeight && action != ActionTranscodeVideo {
		log.Infof("[wrtc %s] quality=%s caps height %d→%d — forcing video transcode",
			agent.ShortID(cfg.SessionID), cfg.Quality, probe.Height, qcap.MaxHeight)
		action = ActionTranscodeVideo
	}

	if action == ActionPassthrough {
		log.Infof("[wrtc %s] codec passthrough (%s + %s in %s)",
			agent.ShortID(cfg.SessionID), probe.VideoCodec, probe.AudioCodec, probe.Container)
		return newDiskFileSource(abs)
	}

	log.Infof("[wrtc %s] transcoding %s/%s/%s → h264+aac (%s, quality=%s)",
		agent.ShortID(cfg.SessionID), probe.Container, probe.VideoCodec, probe.AudioCodec,
		action, coalesce(cfg.Quality, "default"))

	maxHeight := tc.MaxHeight
	videoBitrate := tc.VideoBitrate
	if qcap.MaxHeight > 0 {
		maxHeight = qcap.MaxHeight
		videoBitrate = qcap.VideoBitrate
	}

	opts := TranscodeOpts{
		Action:       action,
		HWAccel:      tc.HWAccel,
		Preset:       tc.Preset,
		VideoBitrate: videoBitrate,
		AudioBitrate: tc.AudioBitrate,
		MaxHeight:    maxHeight,
		SourceHeight: probe.Height,
		FFmpegPath:   tc.FFmpegPath,
	}
	return newTranscodeSource(ctx, abs, probe, action, opts, displayName)
}

// RunWebRTCStream blocks until the session ends — either the DataChannel
// closes, the peer connection drops, or ctx is cancelled. Always returns a
// non-nil error explaining the termination reason.
func RunWebRTCStream(ctx context.Context, cfg WebRTCStreamConfig) error {
	log := logger(cfg.Logger)

	if cfg.SessionID == "" {
		return errors.New("webrtc_stream: empty SessionID")
	}
	if cfg.FilePath == "" {
		return errors.New("webrtc_stream: empty FilePath")
	}

	abs, err := filepath.Abs(cfg.FilePath)
	if err != nil {
		return fmt.Errorf("webrtc_stream: resolve path: %w", err)
	}

	displayName := cfg.FileName
	if displayName == "" {
		displayName = filepath.Base(abs)
	}

	// Decide passthrough vs transcoding. Probe is best-effort: if ffprobe
	// is missing or fails we fall back to passthrough (the browser will
	// surface a clearer error than us guessing wrong).
	source, err := buildStreamSource(ctx, abs, displayName, cfg, log)
	if err != nil {
		return fmt.Errorf("webrtc_stream: build source: %w", err)
	}
	defer source.Close()

	// 1. Build PeerConnection.
	api := webrtc.NewAPI()
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: cfg.ICEServers,
	})
	if err != nil {
		return fmt.Errorf("webrtc_stream: new peer connection: %w", err)
	}
	defer pc.Close()

	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	// Stop the session when ICE drops permanently. "Disconnected" is
	// transient per RFC 8445 (NAT rebind, brief packet loss) — wait for
	// "Failed" or "Closed" before tearing down.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Infof("[wrtc %s] ice=%s", agent.ShortID(cfg.SessionID), state.String())
		switch state {
		case webrtc.ICEConnectionStateFailed,
			webrtc.ICEConnectionStateClosed:
			cancelSession()
		case webrtc.ICEConnectionStateUnknown,
			webrtc.ICEConnectionStateNew,
			webrtc.ICEConnectionStateChecking,
			webrtc.ICEConnectionStateConnected,
			webrtc.ICEConnectionStateCompleted,
			webrtc.ICEConnectionStateDisconnected:
			// Disconnected is transient (RFC 8445 — NAT rebind / packet loss);
			// the others are normal progress states. Don't tear the session down.
		}
	})

	// Trickle our ICE candidates back to the browser.
	// PostSignal runs on its own goroutine so a slow signal server can't
	// stall pion's ICE-gathering thread.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			go func() {
				_ = cfg.Signal.PostSignal(sessionCtx, cfg.SessionID, agent.SignalMessage{
					Type:    agent.SignalMsgCandidateEnd,
					Payload: "",
				})
			}()
			return
		}
		init := c.ToJSON()
		payload, _ := json.Marshal(init)
		go func() {
			_ = cfg.Signal.PostSignal(sessionCtx, cfg.SessionID, agent.SignalMessage{
				Type:    agent.SignalMsgCandidate,
				Payload: string(payload),
			})
		}()
	})

	// Browser is the offerer — we react to the DataChannel it creates.
	dcReady := make(chan *webrtc.DataChannel, 1)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Infof("[wrtc %s] data channel '%s' open", agent.ShortID(cfg.SessionID), dc.Label())
		select {
		case dcReady <- dc:
		default:
			// Browser opened a second DC — ignore, we only serve one.
			log.Warnf("[wrtc %s] extra data channel ignored", agent.ShortID(cfg.SessionID))
		}
	})

	// 2. Drive the SDP exchange. Any error from the loop (browser sent
	// "bye", signal stream closed, etc.) cancels the session so we don't
	// dangle on the DC waiting for a peer that's already gone.
	sdpDone := make(chan error, 1)
	go func() {
		err := runSDPExchange(sessionCtx, pc, cfg)
		sdpDone <- err
		if err != nil && sessionCtx.Err() == nil {
			log.Infof("[wrtc %s] signal loop ended: %v", agent.ShortID(cfg.SessionID), err)
			cancelSession()
		}
	}()

	// 3. Wait for either SDP error or DataChannel open.
	var dc *webrtc.DataChannel
	select {
	case err := <-sdpDone:
		if err != nil {
			return fmt.Errorf("sdp exchange: %w", err)
		}
		// SDP complete — wait for the DC.
		select {
		case dc = <-dcReady:
		case <-time.After(helloDeadline):
			return errors.New("webrtc_stream: data channel never opened")
		case <-sessionCtx.Done():
			return sessionCtx.Err()
		}
	case dc = <-dcReady:
		// DC opened before SDP loop reported done (typical: the loop keeps
		// running to ferry remote ICE candidates).
	case <-sessionCtx.Done():
		return sessionCtx.Err()
	}

	// 4. Wire up the data channel pump.
	pump := newDataChannelPump(dc, source, log, cancelSession)
	dc.OnOpen(pump.onOpen)
	dc.OnMessage(pump.onMessage)
	dc.OnClose(func() {
		log.Infof("[wrtc %s] data channel closed", agent.ShortID(cfg.SessionID))
		cancelSession()
	})

	<-sessionCtx.Done()
	pump.shutdown()
	return sessionCtx.Err()
}

// runSDPExchange consumes signal events from the browser and answers the SDP
// offer. Keeps running for the lifetime of sessionCtx so trickle candidates
// flow in both directions. Reopens the SSE stream on every clean close — the
// server caps each response at ~25 s.
func runSDPExchange(ctx context.Context, pc *webrtc.PeerConnection, cfg WebRTCStreamConfig) error {
	gotOffer := false
	for ctx.Err() == nil {
		stream, err := cfg.Signal.OpenSignalStream(ctx, cfg.SessionID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("open signal stream: %w", err)
		}
		err = consumeSignalStream(ctx, pc, cfg, stream, &gotOffer)
		stream.Close()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// consumeSignalStream drains a single SSE connection until it closes or
// produces a hard error. Returns nil on a clean server-side disconnect so the
// caller can reopen.
func consumeSignalStream(
	ctx context.Context,
	pc *webrtc.PeerConnection,
	cfg WebRTCStreamConfig,
	stream *agent.SignalEventStream,
	gotOffer *bool,
) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-stream.Events():
			if !ok {
				if err := stream.Err(); err != nil {
					return fmt.Errorf("signal stream: %w", err)
				}
				return nil
			}
			if err := handleSignal(ctx, pc, cfg, msg, gotOffer); err != nil {
				return err
			}
		}
	}
}

func handleSignal(
	ctx context.Context,
	pc *webrtc.PeerConnection,
	cfg WebRTCStreamConfig,
	msg agent.SignalMessage,
	gotOffer *bool,
) error {
	switch msg.Type {
	case agent.SignalMsgAnswer:
		// Browser is the offerer in our protocol — we never expect an answer
		// from the other side. Drop silently (also satisfies exhaustive lint).
		return nil
	case agent.SignalMsgOffer:
		if *gotOffer {
			return nil // ignore duplicates
		}
		var offer webrtc.SessionDescription
		if err := json.Unmarshal([]byte(msg.Payload), &offer); err != nil {
			return fmt.Errorf("decode offer: %w", err)
		}
		if err := pc.SetRemoteDescription(offer); err != nil {
			return fmt.Errorf("set remote description: %w", err)
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			return fmt.Errorf("create answer: %w", err)
		}
		if err := pc.SetLocalDescription(answer); err != nil {
			return fmt.Errorf("set local description: %w", err)
		}
		// Send back the local description *with* gathered candidates so far —
		// remaining candidates trickle separately via OnICECandidate.
		ld := pc.LocalDescription()
		payload, _ := json.Marshal(ld)
		if err := cfg.Signal.PostSignal(ctx, cfg.SessionID, agent.SignalMessage{
			Type:    agent.SignalMsgAnswer,
			Payload: string(payload),
		}); err != nil {
			return fmt.Errorf("post answer: %w", err)
		}
		*gotOffer = true

	case agent.SignalMsgCandidate:
		if !*gotOffer {
			// Browser may trickle candidates before we've seen the offer in
			// rare race conditions — drop. Browser will retransmit.
			return nil
		}
		var init webrtc.ICECandidateInit
		if err := json.Unmarshal([]byte(msg.Payload), &init); err != nil {
			return fmt.Errorf("decode candidate: %w", err)
		}
		if err := pc.AddICECandidate(init); err != nil {
			return fmt.Errorf("add ice candidate: %w", err)
		}

	case agent.SignalMsgCandidateEnd:
		// No-op — pion gathers complete on its own.

	case agent.SignalMsgBye:
		return errors.New("browser sent bye")
	}
	return nil
}

// dataChannelPump owns the DC + stream source and serves wire-protocol frames.
type dataChannelPump struct {
	dc     *webrtc.DataChannel
	source streamSource
	log    StreamLogger
	cancel context.CancelFunc

	// Flow control: writers wait on resumeCh when bufferedAmount goes high.
	paused   atomic.Bool
	resumeCh chan struct{}

	// Active range responses keyed by stream_id so CANCEL frames can stop them.
	activeMu sync.Mutex
	active   map[uint32]context.CancelFunc

	// Bound concurrent in-flight responses.
	sem chan struct{}

	// closed once shutdown() has been called.
	closed atomic.Bool
}

func newDataChannelPump(
	dc *webrtc.DataChannel,
	source streamSource,
	log StreamLogger,
	cancel context.CancelFunc,
) *dataChannelPump {
	p := &dataChannelPump{
		dc:       dc,
		source:   source,
		log:      log,
		cancel:   cancel,
		resumeCh: make(chan struct{}, 1),
		active:   make(map[uint32]context.CancelFunc),
		sem:      make(chan struct{}, rangeReqConcurrency),
	}
	dc.SetBufferedAmountLowThreshold(dcLowWatermark)
	dc.OnBufferedAmountLow(p.onBufferedAmountLow)
	return p
}

func (p *dataChannelPump) onOpen() {
	// Use estimated size for transcoded streams so the browser scrubber has
	// something to anchor on. Real size is reflected by Range responses as
	// ffmpeg writes more bytes; the estimate just bootstraps the UI.
	announceSize := p.source.EstimatedSize()
	transcoding := p.source.Transcoded()
	// Browsers refuse to start playback when Content-Length is 0. If we don't
	// have a duration estimate (e.g. ffprobe couldn't tag the source), declare
	// a large sentinel so the browser issues range requests; the Transcoding
	// flag tells it the value is provisional.
	if transcoding && announceSize <= 0 {
		announceSize = math.MaxInt64
	}
	// Seekable=true even for transcoded sources because we read from a tmp
	// file (random access). Seek backwards just works; seek forward beyond
	// what ffmpeg has produced will block briefly inside ReadAt.
	seekable := true
	hello := wire.HelloPayload{
		FileSize:    uint64(announceSize),
		Transcoding: transcoding,
		Seekable:    seekable,
		FileName:    p.source.FileName(),
	}
	payload := wire.EncodeHello(hello)
	frame := wire.EncodeFrame(wire.Header{
		Type:     wire.FrameHello,
		Flags:    wire.HelloFlags(transcoding, seekable),
		StreamID: 0,
		Length:   uint32(len(payload)),
	}, payload)
	if err := p.dc.Send(frame); err != nil {
		p.log.Errorf("send hello: %v", err)
		p.cancel()
	}
}

func (p *dataChannelPump) onMessage(msg webrtc.DataChannelMessage) {
	if len(msg.Data) < wire.HeaderSize {
		p.log.Warnf("dc: short frame %d bytes", len(msg.Data))
		return
	}
	hdr, err := wire.DecodeHeader(msg.Data[:wire.HeaderSize])
	if err != nil {
		p.log.Warnf("dc: bad header: %v", err)
		return
	}
	payload := msg.Data[wire.HeaderSize:]
	if uint32(len(payload)) != hdr.Length {
		p.log.Warnf("dc: payload length mismatch: hdr=%d got=%d", hdr.Length, len(payload))
		return
	}

	switch hdr.Type {
	case wire.FrameRangeReq:
		req, err := wire.DecodeRangeReq(payload)
		if err != nil {
			p.log.Warnf("dc: bad range_req: %v", err)
			return
		}
		go p.serveRange(hdr.StreamID, req)
	case wire.FrameCancel:
		p.cancelStream(hdr.StreamID)
	case wire.FramePing:
		p.sendSimpleFrame(wire.FramePong, hdr.StreamID, nil)
	case wire.FramePong:
		// no-op
	default:
		p.log.Warnf("dc: unknown frame type 0x%02x", hdr.Type)
	}
}

func (p *dataChannelPump) cancelStream(streamID uint32) {
	p.activeMu.Lock()
	cancel, ok := p.active[streamID]
	delete(p.active, streamID)
	p.activeMu.Unlock()
	if ok {
		cancel()
	}
}

func (p *dataChannelPump) sendSimpleFrame(t wire.FrameType, streamID uint32, payload []byte) {
	frame := wire.EncodeFrame(wire.Header{
		Type:     t,
		StreamID: streamID,
		Length:   uint32(len(payload)),
	}, payload)
	if err := p.dc.Send(frame); err != nil {
		p.log.Warnf("dc: send type=0x%02x: %v", t, err)
	}
}

func (p *dataChannelPump) serveRange(streamID uint32, req wire.RangeReqPayload) {
	if p.closed.Load() {
		return
	}
	// Bound concurrency.
	select {
	case p.sem <- struct{}{}:
	case <-time.After(5 * time.Second):
		p.log.Warnf("dc: range_req sid=%d dropped (concurrency cap)", streamID)
		p.sendRangeEnd(streamID, 1)
		return
	}
	defer func() { <-p.sem }()

	// Reject offsets above MaxInt64 — uint64→int64 narrowing would wrap to a
	// negative value and bypass the bounds check, then ReadAt would be called
	// with a negative offset.
	currentSize := p.source.Size()
	finalSize := p.source.EstimatedSize()
	if req.Offset > math.MaxInt64 {
		p.sendRangeEnd(streamID, 2) // out of range
		return
	}
	// For transcoded streams `currentSize` grows over time; only reject when
	// the offset is past the *estimated* final size.
	if int64(req.Offset) >= finalSize && p.source.Final() {
		p.sendRangeEnd(streamID, 2)
		return
	}

	want := int64(req.Length)
	if req.Length > math.MaxInt64 {
		want = 0 // treat absurd length as "remainder of file"
	}
	// Cap by *final* size, not currentSize. For a still-transcoding stream
	// currentSize grows over time and ReadAt below already blocks until
	// ffmpeg produces the requested bytes (with a deadline). If we cap
	// `want` by currentSize here we'll send an empty RangeEnd whenever the
	// browser asks for bytes faster than ffmpeg writes them — which is
	// always true on the first few seconds — and the browser then aborts
	// playback with "Format error".
	cap := finalSize
	if !p.source.Final() && cap < int64(req.Offset)+1 {
		// Estimate too small: serve as much as the browser asked for and
		// let ReadAt block.
		cap = int64(req.Offset) + want
	}
	if int64(req.Offset) >= cap && p.source.Final() {
		// Past true end of a finished file.
		p.sendRangeEnd(streamID, 0)
		return
	}
	remaining := cap - int64(req.Offset)
	if remaining < 0 {
		remaining = 0
	}
	if want <= 0 || want > remaining {
		want = remaining
	}
	p.log.Infof("dc: range_req sid=%d offset=%d wantReq=%d wantServe=%d currentSize=%d final=%v",
		streamID, req.Offset, req.Length, want, currentSize, p.source.Final())
	if want <= 0 {
		// Only happens for a finished file when offset is at/past EOF.
		p.sendRangeEnd(streamID, 0)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.activeMu.Lock()
	if p.active == nil {
		p.activeMu.Unlock()
		cancel()
		p.sendRangeEnd(streamID, 3)
		return
	}
	p.active[streamID] = cancel
	p.activeMu.Unlock()
	defer func() {
		p.activeMu.Lock()
		delete(p.active, streamID)
		p.activeMu.Unlock()
		cancel()
	}()

	buf := make([]byte, dcChunkPayload)
	offset := int64(req.Offset)
	end := offset + want
	for offset < end {
		if ctx.Err() != nil || p.closed.Load() {
			return
		}
		// Wait if the DC is buffering too much.
		if err := p.waitForLowWater(ctx); err != nil {
			return
		}
		chunkLen := int64(len(buf))
		if end-offset < chunkLen {
			chunkLen = end - offset
		}
		n, rerr := p.source.ReadAt(buf[:chunkLen], offset)
		if n > 0 {
			// EOF on a short read means this is the final chunk — flag it so the
			// browser doesn't wait for more data before processing RangeEnd.
			isLast := offset+int64(n) >= end || rerr == io.EOF
			if err := p.sendRangeData(streamID, buf[:n], isLast); err != nil {
				p.log.Warnf("dc: send range_data sid=%d: %v", streamID, err)
				return
			}
			offset += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			p.log.Errorf("dc: read sid=%d: %v", streamID, rerr)
			p.sendRangeEnd(streamID, 3)
			return
		}
	}
	p.sendRangeEnd(streamID, 0)
}

func (p *dataChannelPump) sendRangeData(streamID uint32, data []byte, last bool) error {
	var flags uint8
	if last {
		flags |= wire.FlagLastChunk
	}
	frame := wire.EncodeFrame(wire.Header{
		Type:     wire.FrameRangeData,
		Flags:    flags,
		StreamID: streamID,
		Length:   uint32(len(data)),
	}, data)
	return p.dc.Send(frame)
}

func (p *dataChannelPump) sendRangeEnd(streamID uint32, status uint32) {
	payload := wire.EncodeRangeEnd(wire.RangeEndPayload{Status: status})
	p.sendSimpleFrame(wire.FrameRangeEnd, streamID, payload)
}

func (p *dataChannelPump) waitForLowWater(ctx context.Context) error {
	if p.dc.BufferedAmount() < dcHighWatermark {
		return nil
	}
	p.paused.Store(true)
	for {
		// Drain any stale resume signal first.
		select {
		case <-p.resumeCh:
		default:
		}
		if p.dc.BufferedAmount() < dcHighWatermark {
			p.paused.Store(false)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.resumeCh:
		case <-time.After(500 * time.Millisecond):
			// Belt-and-braces poll in case OnBufferedAmountLow misses a fire.
		}
	}
}

func (p *dataChannelPump) onBufferedAmountLow() {
	if !p.paused.Load() {
		return
	}
	select {
	case p.resumeCh <- struct{}{}:
	default:
	}
}

func (p *dataChannelPump) shutdown() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	p.activeMu.Lock()
	for _, cancel := range p.active {
		cancel()
	}
	p.active = nil
	p.activeMu.Unlock()
}
