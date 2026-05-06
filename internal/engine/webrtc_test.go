package engine

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/torrentclaw/unarr/internal/config"
)

const validHash = "aaf2c71b0e0a03d3f9b2a3e1d5c6b7a8f0e1d2c3"

// TestBuildMagnet_NoExtras verifies the legacy free-function path keeps
// emitting only the static defaultTrackers list.
func TestBuildMagnet_NoExtras(t *testing.T) {
	got := buildMagnet(validHash)
	if !strings.HasPrefix(got, "magnet:?xt=urn:btih:"+validHash) {
		t.Fatalf("magnet missing xt: %s", got)
	}
	if !strings.Contains(got, url.QueryEscape("udp://tracker.opentrackr.org:1337/announce")) {
		t.Fatal("expected default UDP tracker absent")
	}
	if strings.Contains(got, "wss%3A") {
		t.Fatalf("unexpected WSS tracker leaked when none requested: %s", got)
	}
}

// TestBuildMagnet_WithExtraTrackers verifies extraTrackers (e.g. WebRTC
// WSS endpoints) are prepended before the defaults and properly URL-encoded.
func TestBuildMagnet_WithExtraTrackers(t *testing.T) {
	got := buildMagnet(validHash, "wss://tracker.torrentclaw.com")
	encWss := url.QueryEscape("wss://tracker.torrentclaw.com")
	encUDP := url.QueryEscape("udp://tracker.opentrackr.org:1337/announce")
	if !strings.Contains(got, "tr="+encWss) {
		t.Fatalf("WSS tracker missing: %s", got)
	}
	wssIdx := strings.Index(got, encWss)
	udpIdx := strings.Index(got, encUDP)
	if wssIdx < 0 || udpIdx < 0 || wssIdx > udpIdx {
		t.Fatalf("WSS tracker should appear BEFORE UDP defaults: wss=%d udp=%d", wssIdx, udpIdx)
	}
}

// TestBuildMagnet_TrimsAndSkipsEmpty makes sure callers passing config-derived
// slices with stray whitespace or empty strings don't get malformed magnets.
func TestBuildMagnet_TrimsAndSkipsEmpty(t *testing.T) {
	got := buildMagnet(validHash, "  wss://tracker.torrentclaw.com  ", "", "  ")
	encWss := url.QueryEscape("wss://tracker.torrentclaw.com")
	if !strings.Contains(got, "tr="+encWss) {
		t.Fatalf("trimmed WSS tracker missing: %s", got)
	}
	if strings.Contains(got, "tr=&") || strings.HasSuffix(got, "tr=") {
		t.Fatalf("empty tracker emitted: %s", got)
	}
}

// TestTorrentDownloader_buildMagnet_WebRTCDisabled confirms the downloader
// method does NOT inject WebRTCTrackers when WebRTCEnabled is false.
func TestTorrentDownloader_buildMagnet_WebRTCDisabled(t *testing.T) {
	d := &TorrentDownloader{cfg: TorrentConfig{
		WebRTCEnabled:  false,
		WebRTCTrackers: []string{"wss://tracker.torrentclaw.com"},
	}}
	got := d.buildMagnet(validHash)
	if strings.Contains(got, "wss%3A") {
		t.Fatalf("WSS tracker leaked while WebRTCEnabled=false: %s", got)
	}
}

// TestTorrentDownloader_buildMagnet_WebRTCEnabled confirms the WSS trackers
// are present when WebRTCEnabled is true.
func TestTorrentDownloader_buildMagnet_WebRTCEnabled(t *testing.T) {
	d := &TorrentDownloader{cfg: TorrentConfig{
		WebRTCEnabled:  true,
		WebRTCTrackers: []string{"wss://tracker.torrentclaw.com", "wss://tracker2.example.com"},
	}}
	got := d.buildMagnet(validHash)
	for _, want := range []string{
		"wss://tracker.torrentclaw.com",
		"wss://tracker2.example.com",
	} {
		if !strings.Contains(got, url.QueryEscape(want)) {
			t.Fatalf("expected tracker %q missing in magnet: %s", want, got)
		}
	}
}

// TestBuildICEServers_DisabledReturnsNil ensures we don't leak STUN/TURN
// configuration into the torrent client when the user has WebRTC off.
func TestBuildICEServers_DisabledReturnsNil(t *testing.T) {
	got := BuildICEServers(config.WebRTCConfig{
		Enabled:     false,
		STUNServers: []string{"stun:stun.l.google.com:19302"},
	})
	if got != nil {
		t.Fatalf("expected nil ICE servers when disabled, got %+v", got)
	}
}

// TestBuildICEServers_STUNOnly converts STUN entries to bare ICEServer
// records with no credentials.
func TestBuildICEServers_STUNOnly(t *testing.T) {
	got := BuildICEServers(config.WebRTCConfig{
		Enabled:     true,
		STUNServers: []string{"stun:stun.l.google.com:19302", "", "stun:stun1.l.google.com:19302"},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 STUN servers (empty skipped), got %d (%+v)", len(got), got)
	}
	if got[0].URLs[0] != "stun:stun.l.google.com:19302" {
		t.Fatalf("first server unexpected: %+v", got[0])
	}
	if got[0].Username != "" || got[0].Credential != nil {
		t.Fatalf("STUN entry should have no credentials, got %+v", got[0])
	}
}

// TestNewTorrentDownloader_WebRTCEnabled creates a downloader with the
// WebRTC peer fully wired up and confirms the constructor doesn't error
// (anacrolix accepts the ICE server list, port binds, etc.).
func TestNewTorrentDownloader_WebRTCEnabled(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:        dir,
		ListenPort:     0, // let the OS pick — avoid clashes in CI
		WebRTCEnabled:  true,
		WebRTCTrackers: []string{"wss://tracker.torrentclaw.com"},
		ICEServers: BuildICEServers(config.WebRTCConfig{
			Enabled:     true,
			STUNServers: []string{"stun:stun.l.google.com:19302"},
		}),
	})
	if err != nil {
		t.Fatalf("WebRTC-enabled downloader failed to start: %v", err)
	}
	defer func() {
		if err := dl.Shutdown(context.Background()); err != nil {
			t.Logf("shutdown: %v", err)
		}
	}()

	// Magnet for any task should now contain the WSS tracker.
	got := dl.buildMagnet(validHash)
	if !strings.Contains(got, "wss%3A%2F%2Ftracker.torrentclaw.com") {
		t.Fatalf("WebRTC magnet missing WSS tracker: %s", got)
	}
}

// TestBuildICEServers_TURNWithCreds applies TURNUser/TURNPass to every TURN
// entry so the operator only specifies them once.
func TestBuildICEServers_TURNWithCreds(t *testing.T) {
	got := BuildICEServers(config.WebRTCConfig{
		Enabled:     true,
		STUNServers: []string{"stun:stun.l.google.com:19302"},
		TURNServers: []string{"turn:turn.example.com:3478"},
		TURNUser:    "alice",
		TURNPass:    "s3cr3t",
	})
	if len(got) != 2 {
		t.Fatalf("expected 1 STUN + 1 TURN, got %d", len(got))
	}
	turn := got[1]
	if turn.URLs[0] != "turn:turn.example.com:3478" {
		t.Fatalf("TURN URL wrong: %+v", turn)
	}
	if turn.Username != "alice" {
		t.Fatalf("TURN username wrong: %s", turn.Username)
	}
	if turn.Credential != "s3cr3t" {
		t.Fatalf("TURN credential wrong: %v", turn.Credential)
	}
	if turn.CredentialType != webrtc.ICECredentialTypePassword {
		t.Fatalf("TURN credential type wrong: %v", turn.CredentialType)
	}
}
