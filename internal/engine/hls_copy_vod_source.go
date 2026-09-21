package engine

import (
	"log"
	"os"

	"github.com/Unarr-app/unarr-cli/internal/engine/srcproxy"
)

// copyVODDirectEnv disables the loopback source proxy (field kill-switch): every
// COPY-VOD reader then hits the remote URL directly, as before the proxy existed.
const copyVODDirectEnv = "UNARR_COPYVOD_DIRECT"

// startCopySourceProxy fronts a REMOTE source with the range-caching proxy so the
// per-segment ffmpeg spawns stop re-downloading the container header and seek
// index. Best-effort: on any failure readers fall back to the direct URL.
func (s *HLSSession) startCopySourceProxy() {
	if s.cfg.SourceURL == "" || os.Getenv(copyVODDirectEnv) == "1" {
		return
	}
	p, err := srcproxy.Start(srcproxy.Options{URL: s.cfg.SourceURL, Refresh: s.cfg.RefreshURL, Dir: s.tmpDir})
	if err != nil {
		log.Printf("[hls %s] copy-vod source proxy unavailable, reading direct: %v", shortHLSID(s.cfg.SessionID), err)
		return
	}
	s.copyProxy = p
}

// stopCopySourceProxy closes the proxy (releasing its cache file before the
// session dir is removed — Windows cannot delete an open file) and logs how much
// of the traffic never touched the network.
func (s *HLSSession) stopCopySourceProxy() {
	p := s.copyProxy
	if p == nil {
		return
	}
	st := p.Stats()
	_ = p.Close()
	if st.ServedBytes > 0 {
		log.Printf("[hls %s] copy-vod source proxy: %.1f MB upstream in %d requests, %.1f MB served (%.0f%% from cache)",
			shortHLSID(s.cfg.SessionID), float64(st.UpstreamBytes)/1e6, st.UpstreamRequests,
			float64(st.ServedBytes)/1e6, 100*float64(st.CacheHitBytes)/float64(st.ServedBytes))
	}
}

// copySource is what latency-critical COPY-VOD readers open (segments, index,
// IDR check).
func (s *HLSSession) copySource() string {
	if s.copyProxy != nil {
		return s.copyProxy.ForegroundURL()
	}
	return s.cfg.sourceRef()
}

// copyBulkSource is what readers that must never delay the picture open (the
// subtitle extractor): through the proxy, cache hits are served at once and
// anything that needs the network yields the link to segment generation.
func (s *HLSSession) copyBulkSource() string {
	if s.copyProxy != nil {
		return s.copyProxy.BackgroundURL()
	}
	return s.cfg.sourceRef()
}
