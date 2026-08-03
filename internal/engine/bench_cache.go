package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// encodeBenchCacheSchema is bumped whenever the SHAPE of the cache file
// changes. It rides inside the fingerprint, so an old file is not merely
// ignored — it fails the match and is treated as stale, which is what we want:
// a reader must never decode a v1 body with v2 expectations.
const encodeBenchCacheSchema = 1

// encodeBenchCacheFile lives in the data dir (not the config dir): it is a
// measurement, not a setting, and `unarr clean --all` is allowed to drop it.
const encodeBenchCacheFile = "bench-encode.json"

// EncodeBenchKey is everything that can change the answer. ffmpeg version
// because a rebuild can swap x264 for a different asm path (or add the HW
// encoder that skips the probe entirely); CPU model + core count because that
// IS the thing being measured; platform because the same silicon under a
// different OS/arch build is a different encoder; hwaccel because the hardware
// path returns 2160 without measuring, so a cached "2160" must expire the
// moment the GPU stops being usable.
//
// Every field is stored in the file in readable form NEXT TO the fingerprint,
// so a human (or a future `doctor`) opening the file can see exactly which
// field drifted — a bare hash would only say "stale", never why.
type EncodeBenchKey struct {
	Schema        int    `json:"schema"`
	FFmpegVersion string `json:"ffmpegVersion"`
	CPU           string `json:"cpu"`
	CPUCores      int    `json:"cpuCores"`
	Platform      string `json:"platform"`
	HWAccel       string `json:"hwaccel"`
}

// NewEncodeBenchKey samples the current host. ffmpegVersion is passed in rather
// than probed here because every caller already holds an HWAccelDiagnostic,
// and re-running `ffmpeg -version` for the cache key would cost a subprocess
// to learn something we were just told.
func NewEncodeBenchKey(ffmpegVersion string, hw HWAccel) EncodeBenchKey {
	return EncodeBenchKey{
		Schema:        encodeBenchCacheSchema,
		FFmpegVersion: ffmpegVersion,
		CPU:           cpuModel(),
		CPUCores:      runtime.NumCPU(),
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		HWAccel:       string(hw),
	}
}

// Fingerprint is a stable digest of the key. Comparing digests (rather than
// structs) keeps the freshness check to one string compare and survives new
// fields being added to the struct without a reader having to know them.
func (k EncodeBenchKey) Fingerprint() string {
	canonical := strings.Join([]string{
		"v" + strconv.Itoa(k.Schema), k.FFmpegVersion, k.CPU,
		strconv.Itoa(k.CPUCores), k.Platform, k.HWAccel,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// CachedEncodeBench is the on-disk record. MeasuredAt is not part of the
// freshness rule (a benchmark of unchanged hardware does not rot), but it is
// what lets a reader say "measured 3 weeks ago" instead of implying it just
// ran.
type CachedEncodeBench struct {
	Key         EncodeBenchKey  `json:"key"`
	Fingerprint string          `json:"fingerprint"`
	MeasuredAt  time.Time       `json:"measuredAt"`
	CLIVersion  string          `json:"cliVersion,omitempty"`
	Result      EncodeBenchmark `json:"result"`
}

// Fresh reports whether this record still describes the given host.
func (c CachedEncodeBench) Fresh(key EncodeBenchKey) bool {
	return c.Key.Schema == encodeBenchCacheSchema && c.Fingerprint == key.Fingerprint()
}

// EncodeBenchCachePath is where the record lives. Exported so `clean` and
// tests can reason about the file without duplicating the name.
func EncodeBenchCachePath() string {
	return filepath.Join(config.DataDir(), encodeBenchCacheFile)
}

// LoadEncodeBench returns the cached record and whether it matches this host.
// A missing/corrupt file is not an error worth surfacing — it is simply "no
// measurement yet" — so the record comes back zeroed with fresh=false.
func LoadEncodeBench(key EncodeBenchKey) (CachedEncodeBench, bool) {
	data, err := os.ReadFile(EncodeBenchCachePath())
	if err != nil {
		return CachedEncodeBench{}, false
	}
	var c CachedEncodeBench
	if err := json.Unmarshal(data, &c); err != nil {
		return CachedEncodeBench{}, false
	}
	return c, c.Fresh(key)
}

// SaveEncodeBench persists a fresh measurement. Written via temp+rename so a
// reader (doctor, or a concurrent bench) never sees a half-written file.
func SaveEncodeBench(key EncodeBenchKey, cliVersion string, res EncodeBenchmark) error {
	rec := CachedEncodeBench{
		Key:         key,
		Fingerprint: key.Fingerprint(),
		MeasuredAt:  time.Now().UTC(),
		CLIVersion:  cliVersion,
		Result:      res,
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := EncodeBenchCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// cpuModel returns a human-readable processor name, or "" when the platform
// won't say. Empty is a valid key component: it degrades the cache to
// "ffmpeg + core count + platform", which still invalidates on the changes
// that matter most, rather than failing the whole benchmark over a cosmetic
// string.
func cpuModel() string {
	switch runtime.GOOS {
	case "linux":
		return cpuModelFromProc()
	case "darwin":
		return sysctlCPUBrand()
	case "windows":
		// Set by the OS for every process; no WMI round trip needed.
		return strings.TrimSpace(os.Getenv("PROCESSOR_IDENTIFIER"))
	}
	return ""
}

func cpuModelFromProc() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	// "model name" on x86, "Model" on some ARM boards (a Pi reports neither and
	// falls through to "" — see the cpuModel doc comment).
	for _, line := range strings.Split(string(data), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "model name", "hardware":
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sysctlCPUBrand() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
	winproc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
