package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/library/mediainfo"
	"github.com/spf13/cobra"
)

// benchSelection is which sections to run. No flag = all three; any flag = only
// the ones named, so `--disk` on a NAS never pays for an ffmpeg probe.
type benchSelection struct {
	encode bool
	disk   bool
	net    bool
}

func (s benchSelection) resolve() benchSelection {
	if !s.encode && !s.disk && !s.net {
		return benchSelection{encode: true, disk: true, net: true}
	}
	return s
}

// benchReport is the JSON contract of `unarr bench --json`. Sections are
// pointers so a section that was not requested is absent from the output,
// rather than present-and-zero, which a consumer could not tell from "measured
// 0 MB/s".
type benchReport struct {
	Encode *encodeSection          `json:"encode,omitempty"`
	Disk   *engine.DiskBenchResult `json:"disk,omitempty"`
	Net    *engine.NetBenchResult  `json:"net,omitempty"`
	Notes  []string                `json:"notes,omitempty"`
}

// encodeSection carries the measurement plus the context needed to read it:
// which ffmpeg produced it and where it was cached for `doctor`.
type encodeSection struct {
	FFmpegPath    string                 `json:"ffmpegPath"`
	FFmpegVersion string                 `json:"ffmpegVersion"`
	Result        engine.EncodeBenchmark `json:"result"`
	CachePath     string                 `json:"cachePath,omitempty"`
	CacheError    string                 `json:"cacheError,omitempty"`
}

// encodeProbeTimeout matches the daemon's own budget (3 rungs × 6 s + slack),
// so `bench` cannot report a ceiling the daemon would never have reached.
const encodeProbeTimeout = 20 * time.Second

func newBenchCmd() *cobra.Command {
	var sel benchSelection

	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Measure what this host can actually transcode, write, and serve",
		Long: `Benchmark the three limits that decide how playback behaves on this host.

  encode  How large a video this CPU can software-transcode in real time.
          Hardware encoders (NVENC/QSV/VA-API/VideoToolbox) report 2160p
          without a probe — they all sustain 4K. The result is cached in the
          data dir, keyed on the ffmpeg version and the CPU, so 'unarr doctor'
          can report it without re-running the measurement.

  disk    Sequential write throughput in the download dir, fsync included, so
          the number is the device's and not the page cache's. Writes a single
          temporary file inside that dir and removes it on every exit path.

  net     The running daemon's own /speedtest and /health endpoints. Over
          loopback this measures how fast the agent SERVES bytes, not your
          internet link.

With no flags all three run. Sections degrade rather than fail: no ffmpeg skips
encode, no daemon skips net.`,
		Example: `  unarr bench            # all three
  unarr bench --encode   # transcode ceiling only
  unarr bench --disk     # download-dir write throughput
  unarr bench --net      # daemon /speedtest
  unarr bench --json     # machine-readable`,
		GroupID: "system",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBench(sel.resolve())
		},
	}

	cmd.Flags().BoolVar(&sel.encode, "encode", false, "measure the software transcode ceiling")
	cmd.Flags().BoolVar(&sel.disk, "disk", false, "measure sequential write throughput in the download dir")
	cmd.Flags().BoolVar(&sel.net, "net", false, "measure the running daemon's serving throughput")

	return cmd
}

func runBench(sel benchSelection) error {
	// Own the interrupt so a Ctrl-C mid-disk-benchmark cancels the write loop
	// and lets its deferred cleanup remove the temp file, instead of the
	// default handler killing us with the file still on the user's disk.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := loadConfig()
	var rep benchReport

	if sel.encode {
		section, note := benchEncode(ctx, cfg)
		rep.Encode = section
		rep.Notes = appendNote(rep.Notes, note)
	}
	if sel.disk {
		res, note := benchDisk(ctx, cfg)
		rep.Disk = res
		rep.Notes = appendNote(rep.Notes, note)
	}
	if sel.net {
		res, note := benchNet(ctx, cfg)
		rep.Net = res
		rep.Notes = appendNote(rep.Notes, note)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderBench(rep)
	return nil
}

func appendNote(notes []string, note string) []string {
	if note == "" {
		return notes
	}
	return append(notes, note)
}

// benchEncode runs the transcode probe and refreshes the cache. `bench` is the
// PRODUCER of that cache — it always measures, because a user who typed
// "bench" asked for a measurement — while `doctor` is the consumer that reads
// it instead of costing seconds on every run.
func benchEncode(ctx context.Context, cfg config.Config) (*encodeSection, string) {
	ffmpegPath, err := mediainfo.ResolveFFmpeg(cfg.Library.FFmpegPath)
	if err != nil || ffmpegPath == "" {
		return nil, "encode: ffmpeg not found — install it to measure the transcode ceiling"
	}

	probeCtx, cancel := context.WithTimeout(ctx, encodeProbeTimeout)
	defer cancel()
	diag := engine.DetectHWAccelDiagnostic(probeCtx, ffmpegPath)

	benchCtx, benchCancel := context.WithTimeout(ctx, encodeProbeTimeout)
	defer benchCancel()
	result := engine.MeasureEncodeCeiling(benchCtx, ffmpegPath, diag.Pick)

	section := &encodeSection{
		FFmpegPath:    ffmpegPath,
		FFmpegVersion: diag.FFmpegVersion,
		Result:        result,
		CachePath:     engine.EncodeBenchCachePath(),
	}
	key := engine.NewEncodeBenchKey(diag.FFmpegVersion, diag.Pick)
	if err := engine.SaveEncodeBench(key, Version, result); err != nil {
		// The measurement is still valid and still printed — only the handoff
		// to doctor was lost, so say that and carry on.
		section.CacheError = err.Error()
		return section, "encode: result could not be cached: " + err.Error()
	}
	return section, ""
}

func benchDisk(ctx context.Context, cfg config.Config) (*engine.DiskBenchResult, string) {
	dir := cfg.Download.Dir
	if dir == "" {
		return nil, "disk: no download dir configured — run 'unarr init' or set [downloads] dir"
	}
	res, err := engine.BenchmarkDiskWrite(ctx, dir)
	if err != nil {
		return nil, "disk: " + err.Error()
	}
	return &res, ""
}

func benchNet(ctx context.Context, cfg config.Config) (*engine.NetBenchResult, string) {
	// The configured port, not a live one: the daemon may have shifted to the
	// next free port at boot, in which case the probe fails and we say so
	// rather than silently measuring something else.
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Download.StreamPort)
	res, err := engine.BenchmarkStreamEndpoint(ctx, baseURL)
	if err != nil {
		return nil, "net: " + err.Error() + " — start the daemon with 'unarr start'"
	}
	return &res, ""
}
