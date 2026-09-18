package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

func TestCollectorDoesNotExportPrivateFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("LOCALAPPDATA", dir)
	data := config.DataDir()
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	secrets := []string{"John Private", "john@example.invalid", "192.168.99.44", "2001:db8::1234", "aa:bb:cc:dd:ee:ff", "/Users/JohnPrivate/Private Movie.mkv", "https://private.invalid/torrent?token=SECRET", "my-secret-password", "account-uuid-private"}
	private := strings.Join(secrets, " ")
	log := "2026/09/18 12:33:11 Agent registered: " + private + "\n" + private + "\n2026/09/18 12:34:11 panic: " + private + "\n"
	if err := os.WriteFile(filepath.Join(data, "unarr.log"), []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"status": private, "activeTasks": 7, "agentId": "account-uuid-private", "controlToken": "my-secret-password", "logFile": "/Users/JohnPrivate/Private Movie.mkv", "vpnServer": "192.168.99.44", "version": private}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(data, "daemon.state.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Agent.ID = "11111111-2222-4333-8444-555555555555"
	cfg.Agent.Name = private
	cfg.Auth.APIKey = private
	cfg.Download.Dir = ""
	r, err := Collect(&cfg, private)
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(b), secret) {
			t.Fatalf("private data exported: %q", secret)
		}
	}
	if r.System.AppVersion != "unknown" || r.Service.DaemonState != "unknown" || r.Service.ActiveTasks != 7 {
		t.Fatalf("unsafe or lost typed fields: %+v", r)
	}
	if len(r.Logs[0].Events) != 2 || r.Logs[0].Events[0].Event != "daemon_registered" || r.Logs[0].Events[1].Event != "panic" {
		t.Fatalf("diagnosis lost: %+v", r.Logs[0])
	}
	if strings.Contains(string(b), ":null") {
		t.Fatalf("null fields in report: %s", b)
	}
}

func TestCollectorRejectsInvalidAgentIdentifiersWithoutEchoing(t *testing.T) {
	for _, id := range []string{"", "john@example.invalid", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-2222-4333-8444-555555555555", "urn:uuid:11111111-2222-4333-8444-555555555555"} {
		cfg := config.Default()
		cfg.Agent.ID = id
		if _, err := Collect(&cfg, "1.2.3"); err == nil {
			t.Fatalf("accepted noncanonical UUID %q", id)
		} else if id != "" && strings.Contains(err.Error(), id) {
			t.Fatal("error echoes private input")
		}
	}
}

func TestVersionWhitelist(t *testing.T) {
	for _, tc := range []struct{ value, want string }{{"1.2.3", "1.2.3"}, {"1.2.3-rc.1", "1.2.3-rc.1"}, {"1.2.3-john", "unknown"}, {"1.2.3+secret", "unknown"}, {"1.2.3\njohn@example.invalid", "unknown"}, {"127.0.0.1", "unknown"}, {"12345.2.3", "unknown"}} {
		if got := safeVersion(tc.value, releaseVersion); got != tc.want {
			t.Errorf("%q: %q", tc.value, got)
		}
	}
}

func TestServiceParsersWhitelistNestedAndPrivateValues(t *testing.T) {
	s := Service{State: "unknown"}
	parseLaunchdService(&s, "\tstate = waiting\n\t\tstate = running\n\tpath = /Users/John/private\n\tlast exit code = 42\n")
	if s.State != "failed" || s.LastExitCode == nil || *s.LastExitCode != 42 {
		t.Fatalf("wrong launchd state: %+v", s)
	}
	s = Service{State: "unknown"}
	parseSystemdService(&s, "ActiveState=john@example.invalid\nExecMainStatus=999999\nDescription=private\n")
	if s.State != "unknown" || s.LastExitCode != nil {
		t.Fatalf("unbounded fields: %+v", s)
	}
}

func TestDaemonSnapshotDoesNotClaimDeadProcessIsRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"status":"running","pid":0,"activeTasks":-42,"controlToken":"private"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := Service{DaemonState: "unknown"}
	collectDaemonState(&s, path)
	if s.DaemonState != "stopped" || s.ActiveTasks != 0 {
		t.Fatalf("stale or invalid state: %+v", s)
	}
}
