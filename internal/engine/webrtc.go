package engine

import (
	"github.com/pion/webrtc/v4"
	"github.com/torrentclaw/unarr/internal/config"
)

// BuildICEServers converts a config.WebRTCConfig into the
// []webrtc.ICEServer slice that anacrolix/torrent's webtorrent client
// needs. STUN entries become bare URLs; TURN entries inherit the shared
// TURNUser / TURNPass credentials. Returns nil when WebRTC is disabled.
func BuildICEServers(cfg config.WebRTCConfig) []webrtc.ICEServer {
	if !cfg.Enabled {
		return nil
	}
	var servers []webrtc.ICEServer
	for _, s := range cfg.STUNServers {
		if s == "" {
			continue
		}
		servers = append(servers, webrtc.ICEServer{URLs: []string{s}})
	}
	for _, t := range cfg.TURNServers {
		if t == "" {
			continue
		}
		entry := webrtc.ICEServer{URLs: []string{t}}
		if cfg.TURNUser != "" {
			entry.Username = cfg.TURNUser
			entry.Credential = cfg.TURNPass
			entry.CredentialType = webrtc.ICECredentialTypePassword
		}
		servers = append(servers, entry)
	}
	return servers
}
