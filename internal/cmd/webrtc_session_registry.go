package cmd

import (
	"context"
	"log"
	"sync"

	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/engine"
	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// webrtcRegistry tracks per-session cancel funcs for active custom WebRTC
// streams (engine.RunWebRTCStream goroutines). Each session lives only as
// long as its DataChannel; the registry exists so duplicate sync responses
// don't double-spawn the same session and so daemon shutdown can drain.
var webrtcRegistry = &webrtcSessionRegistry{
	cancels: make(map[string]context.CancelFunc),
}

type webrtcSessionRegistry struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func (r *webrtcSessionRegistry) has(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.cancels[sessionID]
	return ok
}

func (r *webrtcSessionRegistry) add(sessionID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[sessionID] = cancel
}

func (r *webrtcSessionRegistry) remove(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, sessionID)
}

// cancelAllWebRTCSessions cancels every running session. Called on daemon
// shutdown so pion peers and SSE consumers exit cleanly.
func cancelAllWebRTCSessions() {
	webrtcRegistry.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(webrtcRegistry.cancels))
	for _, c := range webrtcRegistry.cancels {
		cancels = append(cancels, c)
	}
	webrtcRegistry.cancels = make(map[string]context.CancelFunc)
	webrtcRegistry.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// stdLogger is a tiny adapter so engine.RunWebRTCStream can log through the
// standard library logger without pulling in a logging dependency.
type stdLogger struct{}

func (stdLogger) Infof(format string, args ...any)  { log.Printf(format, args...) }
func (stdLogger) Warnf(format string, args ...any)  { log.Printf("WARN: "+format, args...) }
func (stdLogger) Errorf(format string, args ...any) { log.Printf("ERROR: "+format, args...) }

// buildTranscodeRuntime resolves the ffmpeg/ffprobe binaries + config knobs
// for the WebRTC streaming pipeline. Failure to resolve a binary returns a
// runtime with empty paths so engine.RunWebRTCStream falls back to
// passthrough — the user gets a clearer codec error from the browser than a
// daemon-side abort.
func buildTranscodeRuntime(ctx context.Context, cfg config.Config) engine.TranscodeRuntime {
	if !cfg.Download.Transcode.Enabled {
		return engine.TranscodeRuntime{Disabled: true}
	}
	ffmpegPath, errF := mediainfo.ResolveFFmpeg(cfg.Library.FFmpegPath)
	ffprobePath, errP := mediainfo.ResolveFFprobe(cfg.Library.FFprobePath)
	if errF != nil || errP != nil {
		return engine.TranscodeRuntime{Disabled: true}
	}
	hw := engine.HWAccelNone
	switch cfg.Download.Transcode.HWAccel {
	case "auto":
		hw = engine.DetectHWAccel(ctx, ffmpegPath)
	case "nvenc":
		hw = engine.HWAccelNVENC
	case "qsv":
		hw = engine.HWAccelQSV
	case "vaapi":
		hw = engine.HWAccelVAAPI
	case "videotoolbox":
		hw = engine.HWAccelVideoToolbox
	case "none", "":
		hw = engine.HWAccelNone
	}
	return engine.TranscodeRuntime{
		FFmpegPath:   ffmpegPath,
		FFprobePath:  ffprobePath,
		HWAccel:      hw,
		Preset:       cfg.Download.Transcode.Preset,
		VideoBitrate: cfg.Download.Transcode.VideoBitrate,
		AudioBitrate: cfg.Download.Transcode.AudioBitrate,
		MaxHeight:    cfg.Download.Transcode.MaxHeight,
	}
}
