package cmd

import (
	"context"
	"sync"

	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/engine"
	"github.com/torrentclaw/unarr/internal/library/mediainfo"
)

// playerSessionRegistry tracks per-session cancel funcs for active in-browser
// HLS streaming sessions. Each session lives only as long as its ffmpeg
// process; the registry exists so duplicate sync responses don't double-spawn
// the same session and so daemon shutdown can drain.
var playerSessionRegistry = &playerSessionRegistryT{
	cancels: make(map[string]context.CancelFunc),
}

type playerSessionRegistryT struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func (r *playerSessionRegistryT) has(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.cancels[sessionID]
	return ok
}

func (r *playerSessionRegistryT) add(sessionID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[sessionID] = cancel
}

func (r *playerSessionRegistryT) remove(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, sessionID)
}

// cancelAllPlayerSessions cancels every running session. Called on daemon
// shutdown so the ffmpeg children and SSE consumers exit cleanly.
func cancelAllPlayerSessions() {
	playerSessionRegistry.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(playerSessionRegistry.cancels))
	for _, c := range playerSessionRegistry.cancels {
		cancels = append(cancels, c)
	}
	playerSessionRegistry.cancels = make(map[string]context.CancelFunc)
	playerSessionRegistry.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// buildTranscodeRuntime resolves the ffmpeg/ffprobe binaries + config knobs
// for the HLS streaming pipeline. Failure to resolve a binary returns a
// runtime with empty paths so the caller can short-circuit instead of
// launching a transcoder that will immediately fail.
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
		// Tonemap HDR→SDR only when this ffmpeg build has zscale; otherwise the
		// filter would error and break playback, so HDR plays untonemapped.
		TonemapHDR: engine.FFmpegSupportsZscale(ffmpegPath),
		// libplacebo (GPU) is preferred over zscale when present — checked here so
		// the per-session arg builder can pick it for HDR sources.
		HasLibplacebo: engine.FFmpegSupportsLibplacebo(ffmpegPath),
	}
}
