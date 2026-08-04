package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// withBenchDataDir points config.DataDir() at a temp dir so a cache test never
// writes to the developer's live ~/.local/share/unarr.
func withBenchDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", dir) // macOS: data dir == config dir
	t.Setenv("XDG_DATA_HOME", dir)    // linux
	t.Setenv("LOCALAPPDATA", dir)     // windows

	got := config.DataDir()
	if !strings.HasPrefix(got, dir) {
		t.Skipf("config.DataDir() = %s on %s, not redirected into %s", got, runtime.GOOS, dir)
	}
	return got
}

func TestEncodeBenchCacheRoundTrip(t *testing.T) {
	dataDir := withBenchDataDir(t)

	key := NewEncodeBenchKey("ffmpeg version 6.1.1", HWAccelNone)
	want := EncodeBenchmark{Ceiling: 720, Threshold: 2.0, Reason: EncodeReasonSustained,
		HWAccel: string(HWAccelNone)}

	if _, fresh := LoadEncodeBench(key); fresh {
		t.Fatal("cache reported fresh before anything was written")
	}
	if err := SaveEncodeBench(key, "1.8.2", want); err != nil {
		t.Fatalf("SaveEncodeBench: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, encodeBenchCacheFile)); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	got, fresh := LoadEncodeBench(key)
	if !fresh {
		t.Fatal("cache not fresh for the key it was written with")
	}
	if got.Result.Ceiling != want.Ceiling || got.Result.Reason != want.Reason {
		t.Errorf("result = %+v, want %+v", got.Result, want)
	}
	if got.MeasuredAt.IsZero() {
		t.Error("measuredAt is zero — a reader cannot tell how old the entry is")
	}
	if got.CLIVersion != "1.8.2" {
		t.Errorf("cliVersion = %q, want 1.8.2", got.CLIVersion)
	}
}

// The whole point of the key: a different ffmpeg, a different CPU, or a
// hardware encoder appearing must invalidate the entry rather than have doctor
// report a number that no longer describes the host.
func TestEncodeBenchCacheInvalidation(t *testing.T) {
	withBenchDataDir(t)

	base := NewEncodeBenchKey("ffmpeg version 6.1.1", HWAccelNone)
	if err := SaveEncodeBench(base, "1.8.2", EncodeBenchmark{Ceiling: 720}); err != nil {
		t.Fatalf("SaveEncodeBench: %v", err)
	}

	cases := map[string]EncodeBenchKey{
		"ffmpeg upgraded": NewEncodeBenchKey("ffmpeg version 7.0", HWAccelNone),
		"gpu appeared":    NewEncodeBenchKey("ffmpeg version 6.1.1", HWAccelNVENC),
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			if _, fresh := LoadEncodeBench(key); fresh {
				t.Error("stale entry reported as fresh")
			}
		})
	}

	// A CPU swap is the same class of change; simulate it by editing the field
	// the live host cannot fake.
	cpuSwap := base
	cpuSwap.CPU = "Some Other CPU"
	if _, fresh := LoadEncodeBench(cpuSwap); fresh {
		t.Error("entry survived a CPU change")
	}

	// And the record must still be readable so a reader can SEE what drifted.
	stale, _ := LoadEncodeBench(cpuSwap)
	if stale.Key.FFmpegVersion != "ffmpeg version 6.1.1" {
		t.Errorf("stale record lost its key fields: %+v", stale.Key)
	}
}

// A cache file from a future/older schema must not be trusted, even if every
// other field happens to line up.
func TestEncodeBenchCacheSchemaMismatchIsStale(t *testing.T) {
	dataDir := withBenchDataDir(t)
	key := NewEncodeBenchKey("ffmpeg version 6.1.1", HWAccelNone)
	if err := SaveEncodeBench(key, "1.8.2", EncodeBenchmark{Ceiling: 720}); err != nil {
		t.Fatalf("SaveEncodeBench: %v", err)
	}

	path := filepath.Join(dataDir, encodeBenchCacheFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(data), `"schema": 1`, `"schema": 99`, 1)
	if bumped == string(data) {
		t.Fatal("schema field not found in the cache file")
	}
	if err := os.WriteFile(path, []byte(bumped), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, fresh := LoadEncodeBench(key); fresh {
		t.Error("a record from another schema was accepted as fresh")
	}
}

func TestEncodeBenchCacheCorruptFileIsNotFresh(t *testing.T) {
	dataDir := withBenchDataDir(t)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, encodeBenchCacheFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, fresh := LoadEncodeBench(NewEncodeBenchKey("x", HWAccelNone)); fresh {
		t.Error("corrupt cache reported as fresh")
	}
}
