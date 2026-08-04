//go:build e2e

// Package e2e drives the log ring and the config round-trip end to end, over
// real files on a real filesystem instead of fakes: rotation only fails in the
// ways it fails on disk (a rename that leaves the old inode behind, a slot that
// never gets dropped), and the `unarr logs` reader is only proven by the binary
// reading a seeded file.
//
// Build-tagged because these tests write megabytes and compile the CLI; `make
// test` stays fast. Run them with:
//
//	go test -tags e2e -race -count=1 ./test/e2e/...
//
// Set UNARR_E2E_DIR to keep the workspaces (and their logs) for inspection:
//
//	UNARR_E2E_DIR=/tmp/unarr-e2e go test -tags e2e ./test/e2e/...
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/logging"
)

// workspace returns a private directory for one test. With UNARR_E2E_DIR set it
// is a named directory under it that SURVIVES the run — the evidence (which
// rotated slots exist, how big they are) is the point of these tests, and
// t.TempDir() would delete it before anyone could look. Without the env var it
// falls back to t.TempDir() so CI leaves nothing behind.
func workspace(t *testing.T) string {
	t.Helper()
	base := os.Getenv("UNARR_E2E_DIR")
	if base == "" {
		return t.TempDir()
	}
	dir := filepath.Join(base, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("clear workspace %s: %v", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create workspace %s: %v", dir, err)
	}
	return dir
}

// sandbox is a workspace with the env an unarr invocation reads already pointed
// into it, so no test can touch the real ~/.config/unarr or ~/.local/share.
type sandbox struct {
	root    string // workspace root
	home    string // HOME — also decides whether service.Respawns() sees a unit
	dataDir string // XDG_DATA_HOME/unarr — where unarr.log lives
	cfgPath string // config.toml passed with --config
}

// newSandbox builds the directory layout an unarr install has and returns the
// paths the test seeds.
func newSandbox(t *testing.T) sandbox {
	t.Helper()
	root := workspace(t)
	s := sandbox{
		root:    root,
		home:    filepath.Join(root, "home"),
		dataDir: filepath.Join(root, "data", "unarr"),
		cfgPath: filepath.Join(root, "config", "config.toml"),
	}
	for _, d := range []string{s.home, s.dataDir, filepath.Dir(s.cfgPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("create %s: %v", d, err)
		}
	}
	return s
}

// logPath is the live daemon log inside the sandbox — the same file
// config.DataDir()+"/unarr.log" resolves to with this env.
func (s sandbox) logPath() string { return filepath.Join(s.dataDir, "unarr.log") }

// env is the environment an `unarr` child gets: an empty HOME (so no installed
// systemd unit diverts `unarr logs` to journalctl) and XDG dirs inside the
// sandbox.
func (s sandbox) env() []string {
	return append(os.Environ(),
		"HOME="+s.home,
		"XDG_DATA_HOME="+filepath.Join(s.root, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(s.root, "config-xdg"),
		"UNARR_CONFIG_DIR="+filepath.Dir(s.cfgPath),
		"NO_COLOR=1",
	)
}

// writeConfig writes a config.toml into the sandbox.
func (s sandbox) writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(s.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// cliBinary compiles cmd/unarr once per test binary and returns its path. The
// CLI cases are about what the SHIPPED command does with its flags, so they go
// through a real process rather than calling runLogs in-process.
func cliBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		root, err := repoRoot()
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "unarr-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "unarr")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/unarr")
		cmd.Dir = root
		if out, berr := cmd.CombinedOutput(); berr != nil {
			buildErr = fmt.Errorf("build unarr: %w\n%s", berr, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return binPath
}

// repoRoot walks up from the package directory to the module root.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// ringSlot is the file holding ring slot i: the live log at 0, the n-th rotated
// sibling above that. Naming the rotated slots is logging.RotatedPath's job —
// spelling the suffix out here would be a second definition of the ring's
// layout, free to drift away from the one the code under test uses.
func ringSlot(path string, i int) string {
	if i == 0 {
		return path
	}
	return logging.RotatedPath(path, i)
}

// ringListing renders the log ring the way `ls -l` would, for test evidence.
// Missing slots are listed too — "unarr.log.4  (absent)" is exactly the fact a
// retention assertion is about.
func ringListing(path string, keep int) string {
	var b strings.Builder
	for i := 0; i <= keep+1; i++ {
		p := ringSlot(path, i)
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s\t%d bytes\n", filepath.Base(p), fi.Size())
			continue
		}
		fmt.Fprintf(&b, "%s\t(absent)\n", filepath.Base(p))
	}
	return b.String()
}

// ringLines reads every line of the ring, newest slot first, and returns them
// in one slice — what a reader walking back through the rotated files sees.
func ringLines(t *testing.T, path string, keep int) []string {
	t.Helper()
	var all []string
	for i := 0; i <= keep; i++ {
		p := ringSlot(path, i)
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", p, err)
		}
		for _, ln := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if ln != "" {
				all = append(all, ln)
			}
		}
	}
	return all
}

// ringBytes is the total size of every file of the ring — the number the whole
// rotation feature exists to bound.
func ringBytes(path string, keep int) int64 {
	var total int64
	for i := 0; i <= keep+1; i++ {
		if fi, err := os.Stat(ringSlot(path, i)); err == nil {
			total += fi.Size()
		}
	}
	return total
}
