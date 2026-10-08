package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// nativeMountFixture uses the production web-account, catalog, DAV and ranged
// CDN path. Its keys, media and servers are exclusively synthetic and loopback.
type nativeMountFixture struct {
	t                                   *testing.T
	cfg                                 config.Config
	files                               map[string][]byte
	web, cdn                            *httptest.Server
	access, apiOutage, cdnOutage        atomic.Int32
	generation, resolves, media, active atomic.Int32
	blocked, cancelled                  chan struct{}
	blockOnce, cancelOnce               sync.Once
	key                                 atomic.Value
	renewedResolves                     atomic.Int32
	startup                             *nativeStartupGates
}

func newNativeMountFixture(t *testing.T) *nativeMountFixture {
	return newNativeMountFixtureWithStartup(t, nil)
}

func newNativeMountFixtureWithStartup(t *testing.T, startup *nativeStartupGates) *nativeMountFixture {
	t.Helper()
	f := &nativeMountFixture{t: t, startup: startup, files: make(map[string][]byte), blocked: make(chan struct{}), cancelled: make(chan struct{})}
	for i, name := range []string{"space name.mkv", "xml & apostrophe's.mkv", "percent% hash#.mkv", "café 日本語.mkv", "renewal.mkv", "blocked.mkv"} {
		data := make([]byte, 2<<20)
		for j := range data {
			data[j] = byte((j*17 + j/4093 + i*29) % 251)
		}
		f.files["Release [123]/"+name] = data
	}
	f.generation.Store(1)
	f.key.Store("native-fixture-device-key")
	f.cdn = httptest.NewServer(http.HandlerFunc(f.serveMedia))
	f.web = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	t.Cleanup(f.web.Close)
	t.Cleanup(f.cdn.Close)
	f.cfg = config.Default()
	f.cfg.Auth.APIURL, f.cfg.Auth.APIKey = f.web.URL, "native-fixture-device-key"
	// Explicitly persist only the loopback mirror; Load must not add public defaults.
	f.cfg.Auth.Mirrors = []string{f.web.URL}
	disabled := false
	f.cfg.Telemetry.Enabled, f.cfg.Daemon.AutoUpgrade = &disabled, &disabled
	f.cfg.General.Country = "ES"
	f.cfg.Mount = config.MountConfig{Enabled: true, Listen: "127.0.0.1:0", RefreshInterval: "10s", CacheDir: filepath.Join(t.TempDir(), "catalog"), NZBDir: filepath.Join(t.TempDir(), "nzbs")}
	return f
}

func (f *nativeMountFixture) serveAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// Production mirror discovery is public. Keep it on the fixture instead of
	// accidentally introducing a ten-second external fallback into readiness.
	if r.URL.Path == "/api/v1/mirrors" {
		if f.startup != nil && !f.startup.waitMirror(r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"mirrors": []map[string]any{{"url": f.web.URL, "primary": true}}})
		return
	}
	if f.apiOutage.Load() != 0 {
		http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.key.Load().(string) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "agent_key_mismatch", "error": "synthetic stale identity"})
		return
	}
	if code := f.access.Load(); code != 0 {
		w.WriteHeader(int(code))
		codeName := "mount_paid_required"
		if code == http.StatusGone {
			codeName = "agent_revoked"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"code": codeName, "error": "synthetic access denied"})
		return
	}
	switch r.URL.Path {
	case "/api/internal/agent/mount/access":
		if f.startup != nil && !f.startup.waitAccess(r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"allowed": true})
	case "/api/internal/agent/mount/accounts":
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []agent.MountAccount{{Provider: "torbox", Revision: "fixture-revision"}}})
	case "/api/internal/agent/mount/library":
		var entries []agent.MountEntry
		for name, data := range f.files {
			entries = append(entries, agent.MountEntry{Path: name, Key: name, Size: int64(len(data)), Reference: name})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		_ = json.NewEncoder(w).Encode(agent.MountPage{Entries: entries})
	case "/api/internal/agent/mount/resolve":
		var body struct {
			Reference string `json:"reference"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad fixture request", 400)
			return
		}
		if _, ok := f.files[body.Reference]; !ok {
			http.NotFound(w, r)
			return
		}
		f.resolves.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.cfg.Auth.APIKey {
			f.renewedResolves.Add(1)
		}
		q := url.Values{"file": {body.Reference}, "generation": {strconv.Itoa(int(f.generation.Load()))}}
		_ = json.NewEncoder(w).Encode(map[string]string{"url": f.cdn.URL + "/media?" + q.Encode()})
	default:
		if strings.HasSuffix(r.URL.Path, "/register") {
			if f.startup != nil && !f.startup.waitRegister(r) {
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"user":{"name":"Native Fixture","plan":"pro"},"features":{}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/wake") {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}
}

func (f *nativeMountFixture) serveMedia(w http.ResponseWriter, r *http.Request) {
	f.media.Add(1)
	f.active.Add(1)
	defer f.active.Add(-1)
	if r.Header.Get("Authorization") != "" {
		f.t.Error("device/DAV credentials reached synthetic CDN")
	}
	if f.cdnOutage.Load() != 0 {
		http.Error(w, "synthetic CDN outage", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Query().Get("generation") != strconv.Itoa(int(f.generation.Load())) {
		http.Error(w, "synthetic expired URL", http.StatusForbidden)
		return
	}
	name := r.URL.Query().Get("file")
	data, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, "/blocked.mkv") {
		start, end := int64(0), int64(len(data)-1)
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "fixture requires range", 400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		f.blockOnce.Do(func() { close(f.blocked) })
		<-r.Context().Done()
		f.cancelOnce.Do(func() { close(f.cancelled) })
		return
	}
	http.ServeContent(w, r, name, time.Unix(1700000000, 0), bytes.NewReader(data))
}

func nativeMountOptIn(t *testing.T, kernel bool) {
	t.Helper()
	if os.Getenv("UNARR_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("set UNARR_NATIVE_ACCEPTANCE=1 for synthetic native acceptance")
	}
	if kernel && os.Getenv("UNARR_NATIVE_KERNEL") != "1" {
		t.Skip("native kernel approval/dependencies required: set UNARR_NATIVE_KERNEL=1")
	}
}

func nativeEventually(t *testing.T, limit time.Duration, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out: " + description)
}

func (f *nativeMountFixture) waitCatalog(s *remoteLibrary) {
	nativeEventually(f.t, 5*time.Second, "synthetic web catalog", func() bool {
		file, err := s.catalog.FS.OpenFile(context.Background(), "/debrid/torbox/Release [123]/space name.mkv", os.O_RDONLY, 0)
		if err != nil {
			return false
		}
		_ = file.Close()
		return true
	})
}

func nativeDAVRequest(ctx context.Context, s *remoteLibrary, method, name, byteRange string) (*http.Response, error) {
	u := s.URL + "/dav/" + strings.ReplaceAll(url.PathEscape(name), "%2F", "/")
	r, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	r.SetBasicAuth(s.user, s.password)
	if byteRange != "" {
		r.Header.Set("Range", byteRange)
	}
	return (&http.Client{Timeout: 5 * time.Second}).Do(r)
}

// Exercise the real saved-identity reload without restarting the daemon.
func nativeRenewPersistentIdentity(t *testing.T, f *nativeMountFixture, filePath string, daemonPID int) {
	t.Helper()
	fresh, err := config.Load(config.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	oldResolves := f.renewedResolves.Load()
	fresh.Auth.APIKey = "native-fixture-renewed-device-key"
	f.key.Store(fresh.Auth.APIKey)
	f.generation.Add(1) // Old signed URLs cannot satisfy the mounted read.
	if err := config.Save(fresh, config.FilePath()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var pending <-chan error
	var lastErr error
	defer func() {
		if t.Failed() {
			t.Logf("renewal elapsed=%s new-key resolves=%d last read=%v", time.Since(started), f.renewedResolves.Load()-oldResolves, lastErr)
		}
	}()
	nativeEventually(t, 25*time.Second, "actual persistent mount with saved replacement identity", func() bool {
		if pending == nil {
			done := make(chan error, 1)
			pending = done
			go func() {
				r, err := os.Open(filePath)
				if err == nil {
					buf := make([]byte, 4096)
					var n int
					n, err = r.ReadAt(buf, 987654)
					_ = r.Close()
					if err == nil && (n != len(buf) || !bytes.Equal(buf, f.files["Release [123]/space name.mkv"][987654:991750])) {
						err = fmt.Errorf("renewed mounted range bytes=%d mismatch", n)
					}
				}
				done <- err
			}()
		}
		select {
		case err := <-pending:
			pending = nil
			lastErr = err
			return err == nil && f.renewedResolves.Load() > oldResolves
		default:
			return false
		}
	})
	state := agent.ReadState()
	if state == nil || state.PID != daemonPID || !agent.IsProcessAlive(daemonPID) {
		t.Fatal("saved identity renewal replaced or stopped the daemon")
	}
	t.Logf("actual persistent K1-to-K2 mounted range and fresh resolve after %s; daemon PID=%d unchanged", time.Since(started), daemonPID)
}
