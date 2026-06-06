package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/config"
	"github.com/torrentclaw/unarr/internal/engine"
	"github.com/torrentclaw/unarr/internal/funnel"
	"github.com/torrentclaw/unarr/internal/library"
	"github.com/torrentclaw/unarr/internal/library/mediainfo"
	"github.com/torrentclaw/unarr/internal/usenet/download"
	"github.com/torrentclaw/unarr/internal/vpn"
)

// newStartCmd creates the top-level `unarr start` command.
func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the download daemon (foreground)",
		Long: `Start the unarr daemon in the foreground.

Registers with the server, receives download tasks via periodic sync,
and executes them using the configured download method.
Supports torrent, debrid, and usenet downloads concurrently.

The daemon syncs state with the server every 3s when someone is viewing
the web dashboard, or every 60s when idle. Press Ctrl+C to stop
gracefully — active downloads get up to 30 seconds to finish.

Requires: API key, agent ID, and download directory (run 'unarr init' first).

To run as a background service, use 'unarr daemon install' instead.`,
		Example: `  unarr start
  unarr start --config /path/to/config.toml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStart()
		},
	}
}

// newStopCmd creates the top-level `unarr stop` command.
func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Long: `Stop the unarr daemon gracefully.

Reads the daemon PID from the state file and sends a graceful stop signal.
Works regardless of whether the daemon was started in the foreground or as a service.

To stop a service-managed daemon and prevent auto-restart, use 'unarr daemon stop' instead.`,
		Example: `  unarr stop`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return stopDaemonByPID()
		},
	}
}

// newDaemonCmd creates `unarr daemon` for administrative subcommands.
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon <command>",
		Short: "Manage the daemon as a system service",
		Long: `Install, control and inspect the unarr daemon as a system service.

  Linux:   systemd user service (~/.config/systemd/user/unarr.service)
  macOS:   launchd agent (~/Library/LaunchAgents/com.torrentclaw.unarr.plist)
  Windows: Task Scheduler task (runs at logon)`,
		Example: `  unarr daemon install
  unarr daemon start
  unarr daemon status
  unarr daemon logs -f
  unarr daemon reload
  unarr daemon restart
  unarr daemon stop
  unarr daemon uninstall`,
	}

	cmd.AddCommand(
		newDaemonInstallCmdReal(),
		newDaemonUninstallCmdReal(),
		newDaemonStartCmd(),
		newDaemonStopCmd(),
		newDaemonRestartCmd(),
		newDaemonSvcStatusCmd(),
		newDaemonLogsCmd(),
		newDaemonReloadCmd(),
	)

	return cmd
}

func runDaemonStart() error {
	cfg := loadConfig()
	bold := color.New(color.Bold)

	// Validate config
	if cfg.Auth.APIKey == "" {
		return fmt.Errorf("no API key configured — run 'unarr init' first")
	}
	if cfg.Agent.ID == "" {
		return fmt.Errorf("no agent ID — run 'unarr init' first")
	}
	if cfg.Download.Dir == "" {
		return fmt.Errorf("no download directory — run 'unarr init' first")
	}

	// Validate configured paths are safe
	if err := cfg.ValidatePaths(); err != nil {
		return fmt.Errorf("unsafe configuration: %w", err)
	}

	// Ensure download dir exists
	if err := os.MkdirAll(cfg.Download.Dir, 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}

	// Clean up stale resume files (>7 days old)
	resumeDir := filepath.Join(config.DataDir(), "resume")
	if removed := download.CleanStaleFiles(resumeDir, 7*24*time.Hour); removed > 0 {
		log.Printf("Cleaned %d stale resume file(s)", removed)
	}

	fmt.Println()
	bold.Println("  unarr Daemon")
	fmt.Println()

	userAgent := "unarr/" + Version

	// Probe HW accel + derive a sensible transcode resolution cap. The cap
	// is what the web side uses to decide whether the user should pre-empt
	// transcoding by downloading a smaller version (4K source on a software
	// libx264-only host is the canonical case where pre-download wins).
	//
	// Use the full diagnostic (encoders + devices + ffmpeg version) instead
	// of just the picked backend — the extra fields ride along in the
	// register payload so the web "Diagnose transcoder" modal can show *why*
	// libx264 was selected on a host with a GPU (e.g. brew's ffmpeg without
	// --enable-nvenc). 10 s ceiling so a hung ffmpeg binary can't stall
	// startup forever.
	ffmpegResolved, _ := mediainfo.ResolveFFmpeg(cfg.Library.FFmpegPath)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer probeCancel() // guard against a panic inside DetectHWAccelDiagnostic
	hwDiag := engine.DetectHWAccelDiagnostic(probeCtx, ffmpegResolved)
	log.Println(hwDiag.LogLine())
	hwAccelPick := hwDiag.Pick
	// Measure the real transcode ceiling instead of guessing from the backend.
	// HW encoders return 2160 instantly; a software-only host runs a bounded
	// encode benchmark so a weak NAS/CPU reports the rung it can actually
	// sustain (720/480) and the web side routes oversized sources to an
	// external player instead of a stuttering transcode. This blocks
	// registration on a software host, so it's bounded tight (3 rungs × 6 s =
	// 18 s worst case; <1 s on a capable box that passes the first rung). Own
	// timeout — the 10 s probeCtx above is sized for the quick diagnostic.
	benchCtx, benchCancel := context.WithTimeout(context.Background(), 20*time.Second)
	maxTranscodeHeight := engine.BenchmarkMaxTranscodeHeight(benchCtx, ffmpegResolved, hwAccelPick)
	benchCancel()

	// Warm the tonemap capability caches off the hot path. The libplacebo probe
	// actually RUNS the filter (Vulkan device init ~1.7 s), so doing it lazily
	// in buildTranscodeRuntime would tax the FIRST stream session and risk its
	// setup timeout. A real session arrives seconds-to-minutes after startup, so
	// a background warm has finished by then; if one races in first, the cache's
	// own mutex makes the concurrent cold call safe (both compute the same bool).
	if cfg.Download.Transcode.Enabled && ffmpegResolved != "" {
		go func() {
			engine.FFmpegSupportsLibplacebo(ffmpegResolved)
			engine.FFmpegSupportsZscale(ffmpegResolved)
		}()
	}

	// Create daemon config
	daemonCfg := agent.DaemonConfig{
		AgentID:            cfg.Agent.ID,
		AgentName:          cfg.Agent.Name,
		Version:            Version,
		DownloadDir:        cfg.Download.Dir,
		StreamPort:         cfg.Download.StreamPort,
		LanIP:              engine.LanIP(),
		TailscaleIP:        engine.TailscaleIP(),
		CanDelete:          cfg.Library.AllowDelete,
		ScanPaths:          library.ResolveScanPaths(cfg.Download.Dir, cfg.Organize.MoviesDir, cfg.Organize.TVShowsDir, cfg.Library.ScanPath),
		HWAccel:            string(hwAccelPick),
		MaxTranscodeHeight: maxTranscodeHeight,
		FFmpegVersion:      hwDiag.FFmpegVersion,
		FFmpegPath:         hwDiag.FFmpegPath,
		HWEncoders:         hwDiag.Encoders,
		HWDevices:          hwDiag.Devices,
		AutoUpgrade:        cfg.Daemon.AutoUpgradeEnabled(),
		Downlink:           cfg.Daemon.Downlink,
	}

	// Create HTTP client with mirror failover so a `.com` block-out rolls
	// over to `.to` / .onion without restarting the daemon.
	agentClient := newAgentClientFromConfig(cfg, userAgent)
	log.Printf("Transport: HTTP sync → %s (mirrors: %d)", cfg.Auth.APIURL, len(cfg.Auth.Mirrors))

	// Create daemon
	d := agent.NewDaemon(daemonCfg, agentClient)

	// Start SIGUSR1 reload watcher (unix only, no-op on Windows)
	startReloadWatcher(&ReloadableConfig{Daemon: d})

	// Daemon-scoped context — cancelled on shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse speed limits
	maxDl, _ := config.ParseSpeed(cfg.Download.MaxDownloadSpeed)
	maxUl, _ := config.ParseSpeed(cfg.Download.MaxUploadSpeed)

	// Parse torrent timeouts
	metaTimeout, _ := time.ParseDuration(cfg.Download.MetadataTimeout)
	stallTimeout, _ := time.ParseDuration(cfg.Download.StallTimeout)

	// Parse the seeding time target (0/"" = no time target — ratio-only or forever)
	seedTime, _ := time.ParseDuration(cfg.Download.SeedTime)

	// Create progress reporter — only used for stream tasks (handleStreamTask)
	// The sync goroutine handles all regular progress reporting.
	statusInterval, _ := time.ParseDuration(cfg.Daemon.StatusInterval)
	if statusInterval == 0 {
		statusInterval = 3 * time.Second
	}
	reporter := engine.NewProgressReporter(agentClient, statusInterval)
	reporter.SetWatchingFunc(func() bool { return d.Watching.Load() })

	// Managed-VPN add-on: bring up the in-process WireGuard split-tunnel before
	// the torrent client so peer + tracker traffic routes through it. Failure is
	// non-fatal — log and download in the clear (better than refusing to run).
	var vpnTunnel *vpn.Tunnel
	if cfg.Download.VPN.ConfigFile != "" {
		// Self-hosted / personal-VPN mode: read a local .conf directly.
		raw, rerr := os.ReadFile(cfg.Download.VPN.ConfigFile)
		if rerr != nil {
			log.Printf("[vpn] could not read config_file %q (%v) — downloading in the clear", cfg.Download.VPN.ConfigFile, rerr)
		} else if t, uerr := vpn.Up(string(raw)); uerr != nil {
			log.Printf("[vpn] tunnel failed to start from config_file (%v) — downloading in the clear", uerr)
		} else {
			vpnTunnel = t
			defer vpnTunnel.Close()
			log.Printf("[vpn] managed VPN active (local config_file) — torrent traffic split-tunnelled through WireGuard")
		}
	} else if cfg.Download.VPN.Enabled {
		apiURL := cfg.Auth.APIURL
		if apiURL == "" {
			apiURL = "https://torrentclaw.com"
		}
		fetchCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		conf, ferr := vpn.FetchConfig(fetchCtx, apiURL, cfg.Auth.APIKey, "unarr/"+Version, cfg.Agent.ID, false)
		cancel()
		var fe *vpn.FetchError
		switch {
		case ferr != nil && errors.As(ferr, &fe) && fe.Code == vpn.ErrSlotOnDevice:
			log.Printf("[vpn] the single WireGuard slot is already held by another unarr agent — this one downloads in the clear. To protect this machine too, set up OpenVPN on it (1 agent uses WireGuard, the rest use OpenVPN — up to 10). See https://torrentclaw.com/vpn")
		case ferr != nil:
			log.Printf("[vpn] could not enable VPN (%v) — downloading in the clear", ferr)
		default:
			if t, uerr := vpn.Up(conf); uerr != nil {
				log.Printf("[vpn] tunnel failed to start (%v) — downloading in the clear", uerr)
			} else {
				vpnTunnel = t
				defer vpnTunnel.Close()
				log.Printf("[vpn] managed VPN active — torrent traffic split-tunnelled through WireGuard")
			}
		}
	}

	// Record VPN split-tunnel state for `unarr vpn status`.
	if vpnTunnel != nil {
		mode := "managed"
		if cfg.Download.VPN.ConfigFile != "" {
			mode = "self-hosted"
		}
		d.SetVPNState(true, mode, vpnTunnel.Endpoint)
	}

	// Create torrent downloader
	torrentDl, err := engine.NewTorrentDownloader(engine.TorrentConfig{
		DataDir:            cfg.Download.Dir,
		PieceCompletionDir: config.DataDir(), // keep piece-completion DB off NFS/SMB mounts
		MetadataTimeout:    metaTimeout,
		StallTimeout:       stallTimeout,
		MaxTimeout:         0,
		MaxDownloadRate:    maxDl,
		MaxUploadRate:      maxUl,
		ListenPort:         cfg.Download.ListenPort,
		SeedEnabled:        cfg.Download.SeedEnabled,
		SeedRatio:          cfg.Download.SeedRatio,
		SeedTime:           seedTime,
		VPNTunnel:          vpnTunnel,
	})
	if err != nil {
		return fmt.Errorf("create torrent downloader: %w", err)
	}

	if maxDl > 0 || maxUl > 0 {
		dlStr, ulStr := "unlimited", "unlimited"
		if maxDl > 0 {
			dlStr = formatSpeedLog(maxDl)
		}
		if maxUl > 0 {
			ulStr = formatSpeedLog(maxUl)
		}
		log.Printf("Speed limits: download=%s upload=%s", dlStr, ulStr)
	}

	if cfg.Download.SeedEnabled {
		switch {
		case cfg.Download.SeedRatio > 0 && seedTime > 0:
			log.Printf("[torrent] seeding enabled (stop at ratio %.2f or %s, whichever first)", cfg.Download.SeedRatio, seedTime)
		case cfg.Download.SeedRatio > 0:
			log.Printf("[torrent] seeding enabled (stop at ratio %.2f)", cfg.Download.SeedRatio)
		case seedTime > 0:
			log.Printf("[torrent] seeding enabled (stop after %s)", seedTime)
		default:
			log.Printf("[torrent] seeding enabled (no ratio/time target — seeds until shutdown)")
		}
	}

	// Create debrid downloader
	debridDl := engine.NewDebridDownloader()
	usenetDl := engine.NewUsenetDownloader(agentClient)

	// Pre-flight disk reserve: refuse a download that would leave less than this
	// many bytes free, so a download never fills the filesystem to 0 mid-write.
	minFreeBytes := int64(cfg.Download.MinFreeDiskMB) << 20
	torrentDl.SetMinFreeBytes(minFreeBytes)
	debridDl.SetMinFreeBytes(minFreeBytes)
	usenetDl.SetMinFreeBytes(minFreeBytes)
	log.Printf("[disk] download free-space reserve: %d MiB", cfg.Download.MinFreeDiskMB)

	// Create download manager
	manager := engine.NewManager(engine.ManagerConfig{
		MaxConcurrent: cfg.Download.MaxConcurrent,
		OutputDir:     cfg.Download.Dir,
		Notifications: cfg.Notifications.Enabled,
		Organize: engine.OrganizeConfig{
			Enabled:    cfg.Organize.Enabled,
			MoviesDir:  cfg.Organize.MoviesDir,
			TVShowsDir: cfg.Organize.TVShowsDir,
			OutputDir:  cfg.Download.Dir,
		},
	}, reporter, torrentDl, debridDl, usenetDl)

	// Resume store: persist in-flight downloads so a daemon restart can re-submit
	// them (the downloaders resume the partial data). Wire it before any Submit.
	taskStore := agent.NewActiveTaskStore()
	manager.SetTaskStore(taskStore)

	// Create persistent stream server
	streamSrv := engine.NewStreamServer(cfg.Download.StreamPort)
	streamSrv.SetUPnPEnabled(cfg.Download.EnableUPnP)
	// Wire ffmpeg so /thumbnail can extract single frames for the web's "file
	// characteristics" panel (frames on demand). Empty = thumbnails 503.
	streamSrv.SetFFmpegPath(ffmpegResolved)
	// Write-through cache extracted WebVTT into the hidden ".unarr" sidecar dir so
	// /sub serves instantly (and giant remuxes that exceed the on-demand timeout
	// work once the scan prewarm has filled the cache). Default true.
	streamSrv.SetCacheSubtitles(cfg.Library.CacheSubtitles)
	streamSrv.SetCacheThumbnails(cfg.Library.CacheThumbnails)
	// Tell /trickplay which tile width the scan prewarm built the sprite at (the
	// agent owns the width; the web requests by path only). 0 = disabled → 404.
	trickW := 0
	if cfg.Library.Trickplay.Enabled {
		if trickW = cfg.Library.Trickplay.Width; trickW <= 0 {
			trickW = 240
		}
	}
	streamSrv.SetTrickplayWidth(trickW)
	// Self-heal a host→container base-path skew for the path-scoped handlers
	// (/thumbnail, /trickplay, /sub), mirroring the /stream + /hls remap. Without
	// it, a docker agent whose web DB holds host paths (/mnt/nas/peliculas/…) but
	// mounts that media at /downloads returns 404 for every scrubber frame /
	// trickplay sprite / external subtitle. Same allowed roots + relocate logic.
	// NOTE: relocateUnreachable needs a ≥3-segment path tail, so a FLAT media
	// layout (file directly under the root) is not self-healed here — those
	// sidecars 404 on a docker agent with a host→container skew until a re-scan
	// rewrites the DB path. Same limitation as the /stream self-heal.
	streamSrv.SetPathResolver(func(p string) string {
		p = filepath.Clean(p)
		roots := streamAllowedRoots(cfg)
		if isAllowedStreamPath(p, roots...) {
			return p
		}
		return relocateUnreachable(p, roots) // "" when not locatable → caller 404s
	})
	streamSrv.SetRequireStreamToken(cfg.Download.RequireStreamToken)
	// Report the stream-token signing key ONLY when enforcing, so the web's
	// "secret present → mint HLS token" signal accurately means "this agent
	// verifies tokens". Reporting it with enforcement off would make the web
	// mint HLS path tokens the agent never peels → 404. Set before Register().
	if cfg.Download.RequireStreamToken {
		d.UpdateStreamSecret(streamSrv.StreamSecretHex())
	}
	// CORS extras = operator config + dynamic mirror list from /api/mirrors.
	// Without the mirror merge, a user playing from `torrentclaw.to` (or any
	// future mirror) hits the daemon, gets 200 + body, but no
	// `Access-Control-Allow-Origin` → browser drops the response → player
	// reports "404 todos los canales". Fetching /api/mirrors at startup
	// future-proofs against mirror additions without a CLI rebuild.
	corsExtras := append([]string(nil), cfg.Download.CORSExtraOrigins...)
	corsExtras = append(corsExtras, mirrorCORSOrigins(ctx, cfg, userAgent)...)
	streamSrv.SetCORSAllowedOrigins(corsExtras)

	// HTTPS stream listener (agent-TLS feature): only armed when a certificate is
	// present on disk — without a valid cert there is nothing to serve over TLS,
	// and the HTTP listener + funnel keep working. The future ACME broker writes
	// the cert pair to certs/agent.{crt,key} under the agent state dir.
	if cfg.Download.HTTPSStreamPort > 0 {
		certPath := filepath.Join(config.DataDir(), "certs", "agent.crt")
		keyPath := filepath.Join(config.DataDir(), "certs", "agent.key")
		if err := streamSrv.LoadTLSCertificateFromFiles(certPath, keyPath); err != nil {
			log.Printf("[stream] HTTPS disabled — no usable certificate at %s (%v)", certPath, err)
		} else {
			streamSrv.EnableTLS(cfg.Download.HTTPSStreamPort)
			log.Printf("[stream] HTTPS armed on port %d with certificate %s", cfg.Download.HTTPSStreamPort, certPath)
		}
	}
	// Reap HLS tmpdirs left over from a previous daemon run before we start
	// accepting new sessions. The in-memory registry doesn't survive a
	// restart, so without this disk usage grows unbounded across restarts.
	if err := engine.CleanupHLSOrphanDirs(); err != nil {
		log.Printf("[hls] orphan tmpdir cleanup: %v", err)
	}

	// Persistent HLS segment cache — survives across sessions so re-plays
	// of the same file at the same quality skip ffmpeg entirely. Off when
	// hls_cache.enabled = false; size cap from hls_cache.size_gb; path from
	// hls_cache.dir (defaults to ~/.cache/unarr/hls-cache).
	var hlsCache *engine.HLSCache
	if cfg.Download.HLSCache.Enabled {
		cacheDir := cfg.Download.HLSCache.Dir
		if cacheDir == "" {
			if base, err := os.UserCacheDir(); err == nil {
				cacheDir = filepath.Join(base, "unarr", "hls-cache")
			} else {
				cacheDir = filepath.Join(os.TempDir(), "unarr-hls-cache")
			}
		}
		c, err := engine.NewHLSCache(cacheDir, cfg.Download.HLSCache.SizeGB)
		if err != nil {
			log.Printf("[hls_cache] init failed (%v) — falling back to per-session tmpdirs", err)
		} else {
			hlsCache = c
			hlsCache.StartSweeper(ctx, time.Hour)
			log.Printf("[hls_cache] enabled: dir=%s budget=%dGB", cacheDir, cfg.Download.HLSCache.SizeGB)
		}
	} else {
		log.Printf("[hls_cache] disabled by config — every play re-encodes from scratch")
	}
	if err := streamSrv.Listen(ctx); err != nil {
		return fmt.Errorf("start stream server: %w", err)
	}
	d.UpdateStreamPort(streamSrv.Port())

	// CloudFlare Quick Tunnel — needs the ACTUAL listening port (the
	// configured port may have been busy and bumped). Spawning here ensures
	// cloudflared --url points at the right socket. Failures degrade to
	// Tailscale/LAN only; the supervisor keeps the tunnel up across CF's
	// periodic rotation + transient cloudflared crashes.
	if cfg.Download.Funnel.Enabled {
		go superviseFunnel(ctx, d, streamSrv.Port())
	}

	// Warn at startup if transcode is enabled but ffmpeg/ffprobe are missing.
	// HLS sessions get rejected at runtime (see daemon.go ~line 455), but
	// surfacing it here gives the operator a chance to install ffmpeg before
	// a user hits a confusing "rejected" line in the logs.
	if cfg.Download.Transcode.Enabled {
		if _, err := mediainfo.ResolveFFmpeg(cfg.Library.FFmpegPath); err != nil {
			log.Printf("[hls] transcode enabled but ffmpeg/ffprobe not found — install ffmpeg to use HLS")
		} else if _, err := mediainfo.ResolveFFprobe(cfg.Library.FFprobePath); err != nil {
			log.Printf("[hls] transcode enabled but ffmpeg/ffprobe not found — install ffmpeg to use HLS")
		}
	}

	// Wire sync client callbacks
	sc := d.SyncClient()
	sc.GetFreeSlots = manager.FreeSlots
	sc.GetTaskStates = manager.TaskStates
	d.GetActiveCount = manager.ActiveCount

	// Trigger immediate sync when a download slot frees up
	manager.OnTaskDone = func() { d.TriggerSync() }
	// Event-driven uplink: every status transition (resolving/downloading/
	// verifying/organizing/…) pushes to the server right away instead of waiting
	// for the next adaptive tick. Coalesced by TriggerSync's buffered-1 channel.
	manager.OnStateChange = func() { d.TriggerSync() }

	// Wire: sync receives new tasks → submit to manager or handle stream
	d.OnTasksClaimed = func(tasks []agent.Task) {
		for _, t := range tasks {
			if t.Mode == "stream" {
				if isStreamingTask(t.ID) {
					continue
				}
				cancelStreamContexts()
				streamSrv.ClearFile()
				streamCtx, streamCancel := context.WithCancel(ctx) //nolint:gosec // G118: cancel stored in registry
				streamRegistry.mu.Lock()
				streamRegistry.cancels[t.ID] = streamCancel
				streamRegistry.mu.Unlock()
				go handleStreamTask(streamCtx, t, reporter, cfg, agentClient, streamSrv, func() { d.TriggerSync() })
			} else {
				manager.Submit(ctx, t)
			}
		}
	}

	// Resume downloads interrupted by the previous shutdown/crash. Re-submit
	// each persisted task; its downloader picks up the partial data (torrent via
	// the piece-completion DB, debrid via Range, usenet via its tracker). Done
	// before the sync loop starts; a later web re-dispatch of the same id is
	// deduped by the manager.
	if resume := taskStore.Load(); len(resume) > 0 {
		log.Printf("[resume] re-submitting %d interrupted download(s)", len(resume))
		for _, t := range resume {
			t.ForceStart = false // respect MaxConcurrent on bulk auto-resume
			log.Printf("[resume] %s — %s", agent.ShortID(t.ID), t.Title)
			manager.Submit(ctx, t)
		}
	}

	// Wire: sync receives control signals → act on manager
	d.OnControlAction = func(action, taskID string, deleteFiles bool) {
		switch action {
		case "cancel":
			if deleteFiles {
				manager.CancelAndDeleteFiles(taskID)
			} else {
				manager.CancelTask(taskID)
			}
			cancelStreamTask(taskID)
			if streamSrv.CurrentTaskID() == taskID {
				streamSrv.ClearFile()
			}
		case "pause":
			manager.PauseTask(taskID)
			cancelStreamTask(taskID)
			if streamSrv.CurrentTaskID() == taskID {
				streamSrv.ClearFile()
			}
		case "resume":
			log.Printf("[%s] resume requested, triggering sync", agent.ShortID(taskID))
			d.TriggerSync()
		case "stream":
			if streamSrv.CurrentTaskID() == taskID {
				return
			}
			task := manager.GetTask(taskID)
			if task == nil || task.GetStreamURL() != "" {
				return
			}
			provider, err := torrentDl.GetStreamProvider(taskID)
			if err != nil {
				log.Printf("[%s] stream failed: %v", agent.ShortID(taskID), err)
				return
			}
			cancelStreamContexts()
			streamSrv.SetFile(provider, taskID)
			task.SetStreamURL(streamSrv.URLsJSON())
			log.Printf("[%s] streaming: %s", agent.ShortID(taskID), provider.FileName())

			watchCtx, watchCancel := context.WithCancel(ctx) //nolint:gosec // G118
			streamRegistry.mu.Lock()
			streamRegistry.cancels["watch:"+taskID] = watchCancel
			streamRegistry.mu.Unlock()
			go engine.NewWatchReporter(agentClient, streamSrv, taskID).Run(watchCtx)
		case "stop-stream":
			cancelStreamTask(taskID)
			if streamSrv.CurrentTaskID() == taskID {
				streamSrv.ClearFile()
			}
		}
	}

	// Wire: sync receives file deletion requests from the server
	if cfg.Library.AllowDelete && len(daemonCfg.ScanPaths) > 0 {
		sc.OnDeleteFiles = func(items []agent.LibraryDeleteRequest) []int {
			return library.DeleteFiles(items, daemonCfg.ScanPaths)
		}
	}

	// Wire: sync receives stream requests for completed downloads
	d.OnStreamRequested = func(sr agent.StreamRequest) {
		if streamSrv.CurrentTaskID() == sr.TaskID {
			// Already serving — notify server it's ready
			go func() {
				if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
					TaskID:      sr.TaskID,
					StreamReady: true,
				}); err != nil {
					log.Printf("[%s] stream ready re-notify failed: %v", agent.ShortID(sr.TaskID), err)
				}
			}()
			return
		}

		// reportStreamError tells the web a /stream attempt failed WITHOUT
		// marking the download failed (StreamError, not Status). The web clears
		// streamRequested and surfaces this so the player fails fast with the
		// real reason instead of polling out the 20s "agent didn't respond".
		reportStreamError := func(reason string) {
			go func() {
				if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
					TaskID:      sr.TaskID,
					StreamError: reason,
				}); err != nil {
					log.Printf("[%s] stream error report failed: %v", agent.ShortID(sr.TaskID), err)
				}
			}()
		}

		allowedRoots := streamAllowedRoots(cfg)

		filePath := filepath.Clean(sr.FilePath)
		// Self-heal a base-path mismatch: the web may hand us a path under an old
		// root (e.g. /mnt/nas/peliculas/… from before a binary→docker move) that
		// is now outside our allowed dirs but whose file still exists under a
		// current root (/downloads/…). Remap the path's tail onto an allowed root
		// so playback works immediately; the next re-scan persists the fix to the
		// DB. See docs/plans/unarr-path-resilience.md.
		if !isAllowedStreamPath(filePath, allowedRoots...) {
			if remapped := relocateUnreachable(filePath, allowedRoots); remapped != "" {
				log.Printf("[%s] stream self-heal: remapped %s → %s", agent.ShortID(sr.TaskID), filePath, remapped)
				filePath = remapped
			} else {
				log.Printf("[%s] stream request rejected: path outside allowed dirs: %s", agent.ShortID(sr.TaskID), filePath)
				reportStreamError(fmt.Sprintf("path outside allowed dirs: %s", filePath))
				return
			}
		}
		// os.Stat over NFS can transiently fail (ESTALE/EAGAIN/timeout) right
		// after a remount or under load. Retry a few times before giving up so
		// a hiccup doesn't surface as a spurious "file not found" — this is the
		// root of the intermittent "works on the 3rd try" stream failures.
		var info os.FileInfo
		var statErr error
		for attempt := 0; attempt < 3; attempt++ {
			if info, statErr = os.Stat(filePath); statErr == nil {
				break
			}
			if attempt < 2 {
				time.Sleep(300 * time.Millisecond)
			}
		}
		if statErr != nil {
			// Last resort before failing: the file may simply have moved within
			// an allowed root — try to relocate it by path tail.
			if remapped := relocateUnreachable(filePath, allowedRoots); remapped != "" {
				log.Printf("[%s] stream self-heal: relocated missing %s → %s", agent.ShortID(sr.TaskID), filePath, remapped)
				filePath = remapped
				info, statErr = os.Stat(filePath)
			}
		}
		if statErr != nil {
			log.Printf("[%s] stream request: file not found after retries: %s (%v)", agent.ShortID(sr.TaskID), filePath, statErr)
			reportStreamError(fmt.Sprintf("file not found: %s", filePath))
			return
		}

		if info.IsDir() {
			found := engine.FindVideoFile(filePath)
			if found == "" {
				log.Printf("[%s] stream request: no video file in directory: %s", agent.ShortID(sr.TaskID), filePath)
				reportStreamError(fmt.Sprintf("no video file in directory: %s", filePath))
				return
			}
			filePath = found
			log.Printf("[%s] resolved directory to video file: %s", agent.ShortID(sr.TaskID), filepath.Base(filePath))
		}

		cancelStreamContexts()
		streamSrv.SetFile(engine.NewDiskFileProvider(filePath), sr.TaskID)
		log.Printf("[%s] streaming from disk: %s → %s", agent.ShortID(sr.TaskID), filepath.Base(filePath), streamSrv.URL())

		watchCtx, watchCancel := context.WithCancel(ctx) //nolint:gosec // G118
		streamRegistry.mu.Lock()
		streamRegistry.cancels["watch:"+sr.TaskID] = watchCancel
		streamRegistry.mu.Unlock()
		go engine.NewWatchReporter(agentClient, streamSrv, sr.TaskID).Run(watchCtx)

		go func() {
			if _, err := agentClient.ReportStatus(ctx, agent.StatusUpdate{
				TaskID:      sr.TaskID,
				StreamReady: true,
			}); err != nil {
				log.Printf("[%s] stream ready report failed: %v", agent.ShortID(sr.TaskID), err)
			}
		}()
	}

	// Wire: sync receives HLS streaming session requests. Each session spawns
	// one ffmpeg process and registers its HLS playlist with the StreamServer.
	// Validate FilePath against allowed dirs to prevent path traversal abuse
	// from a compromised server.
	d.OnStreamSession = func(sess agent.StreamSession) {
		if playerSessionRegistry.has(sess.SessionID) {
			return // already running
		}

		// startHLSPlayback starts an HLS encode (local file or debrid URL) and
		// wires it into the StreamServer. Shared by the local-file HLS path and
		// the debrid HLS-from-URL path (hueco #2 / 2b) so both register, probe
		// off the sync loop, and report readiness identically.
		startHLSPlayback := func(hlsCfg engine.HLSSessionConfig, hlsCtx context.Context, hlsCancel context.CancelFunc) {
			playerSessionRegistry.add(hlsCfg.SessionID, hlsCancel)
			go func() {
				hsess, err := engine.StartHLSSession(hlsCtx, hlsCfg)
				if err != nil {
					playerSessionRegistry.remove(hlsCfg.SessionID)
					hlsCancel()
					log.Printf("[hls %s] start failed: %v", agent.ShortID(hlsCfg.SessionID), err)
					return
				}
				streamSrv.HLS().Register(hsess)
				go watchSessionReady(hlsCtx, agentClient, hsess, hlsCfg.SessionID)
			}()
		}

		// Debrid direct-play (hueco #2 / 2a): the source has no local file — the
		// web resolved an HTTPS debrid link (cache-confirmed, browser-native
		// container) and the daemon streams /stream from it via ranged GETs.
		// Runs BEFORE the filePath checks (there is no local path) and needs no
		// ffmpeg. PlayMethod != "hls" distinguishes this from the debrid
		// HLS-from-URL branch below (a non-native container the web wants
		// transcoded). Provider setup does a HEAD, so hand it off to a goroutine
		// to keep the sync loop from blocking other pending actions; register the
		// session up front so a duplicate sync within the setup window is a
		// no-op (matches the HLS branch's handoff rationale).
		if sess.DirectURL != "" && sess.PlayMethod != "hls" {
			playerSessionRegistry.add(sess.SessionID, func() { streamSrv.ClearFile() })
			// refresh re-resolves a fresh debrid link when this one expires
			// mid-stream (hueco #2 / 2c). Bound to the daemon ctx so a shutdown
			// cancels an in-flight refresh.
			refresh := func(rctx context.Context) (string, error) {
				return agentClient.RefreshStreamURL(rctx, sess.SessionID)
			}
			go func() {
				bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				provider, perr := engine.NewDebridFileProvider(bctx, sess.DirectURL, sess.FileName, sess.FileSize, refresh)
				if perr != nil {
					playerSessionRegistry.remove(sess.SessionID)
					log.Printf("[stream %s] debrid provider failed: %v", agent.ShortID(sess.SessionID), perr)
					return
				}
				streamSrv.SetFile(provider, sess.TaskID)
				log.Printf("[stream %s] debrid direct-play: %s (%d bytes)",
					agent.ShortID(sess.SessionID), provider.FileName(), provider.FileSize())
				rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
				defer rcancel()
				if err := agentClient.MarkSessionReady(rctx, sess.SessionID, nil); err != nil {
					log.Printf("[stream %s] mark-ready failed: %v", agent.ShortID(sess.SessionID), err)
				}
			}()
			return
		}

		// Debrid HLS-from-URL (hueco #2 / 2b): the source is debrid-cached but
		// NOT browser-native (mkv/HEVC/…), so the web set playMethod="hls"
		// alongside the DirectURL. ffmpeg transcodes straight from the HTTP URL —
		// no local file, no torrent. Cache is keyed by info_hash (not the
		// per-resolution URL) so a re-play hits the segment cache.
		if sess.DirectURL != "" { // playMethod == "hls" implied (2a returned above)
			tcRuntime := buildTranscodeRuntime(ctx, cfg)
			if tcRuntime.FFmpegPath == "" || tcRuntime.FFprobePath == "" {
				log.Printf("[hls %s] rejected: ffmpeg/ffprobe unavailable (debrid HLS)", agent.ShortID(sess.SessionID))
				return
			}
			hlsCtx, hlsCancel := context.WithCancel(ctx)
			startHLSPlayback(engine.HLSSessionConfig{
				SessionID:         sess.SessionID,
				SourceURL:         sess.DirectURL,
				CacheID:           sess.InfoHash,
				FileName:          sess.FileName,
				Quality:           sess.Quality,
				AudioIndex:        sess.AudioIndex,
				BurnSubtitleIndex: sess.BurnSubtitleIndex,
				Transcode:         tcRuntime,
				Cache:             hlsCache,
				// 2c: refresh the debrid link if it expires mid-transcode; the
				// auto-restart supervisor calls this before relaunching ffmpeg.
				RefreshURL: func(rctx context.Context) (string, error) {
					return agentClient.RefreshStreamURL(rctx, sess.SessionID)
				},
			}, hlsCtx, hlsCancel)
			log.Printf("[hls %s] debrid HLS-from-URL: %s", agent.ShortID(sess.SessionID), sess.FileName)
			return
		}

		filePath := sess.FilePath
		if filePath == "" {
			log.Printf("[hls %s] rejected: empty file path", agent.ShortID(sess.SessionID))
			return
		}
		filePath = filepath.Clean(filePath)
		// Apply the SAME base-path self-heal remap as the raw /stream handler
		// (OnStreamRequest above). Without it, a path under an old/host base
		// (e.g. /mnt/nas/peliculas/… handed by the web while this docker agent
		// mounts that media at /downloads) is rejected here even though the raw
		// path self-heals it — so the web silently falls back to the raw stream
		// and HLS/remux never runs (no transcode, slow funnel start). NOTE: this
		// replicates only the lexical-remap; the raw handler additionally retries
		// os.Stat for transient NFS errors. The HLS dir-check below proceeds (not
		// rejects) on a stat error, so it tolerates an NFS blip differently.
		// See docs/plans/unarr-path-resilience.md.
		hlsAllowedRoots := streamAllowedRoots(cfg)
		if !isAllowedStreamPath(filePath, hlsAllowedRoots...) {
			if remapped := relocateUnreachable(filePath, hlsAllowedRoots); remapped != "" {
				log.Printf("[hls %s] self-heal: remapped %s → %s",
					agent.ShortID(sess.SessionID), filePath, remapped)
				filePath = remapped
			} else {
				log.Printf("[hls %s] rejected: path outside allowed dirs: %s",
					agent.ShortID(sess.SessionID), filePath)
				return
			}
		}
		// Resolve directory → first video file (matches StreamRequest behavior).
		if info, err := os.Stat(filePath); err == nil && info.IsDir() {
			found := engine.FindVideoFile(filePath)
			if found == "" {
				log.Printf("[hls %s] rejected: no video file in dir %s",
					agent.ShortID(sess.SessionID), filePath)
				return
			}
			filePath = found
		}

		// Direct-play (hueco #3 / 3a): the web decided this source is already
		// browser-native (mp4 h264/aac 8-bit SDR) from library scan metadata,
		// gated on agent version. Serve the raw file over /stream (HTTP Range,
		// no ffmpeg) instead of transcoding to HLS — zero CPU, instant seek.
		// Runs BEFORE the ffmpeg-availability check on purpose: direct-play
		// needs no ffmpeg, so it must work even when transcode is disabled.
		if sess.PlayMethod == "direct" {
			streamSrv.SetFile(engine.NewDiskFileProvider(filePath), sess.TaskID)
			// cancel just clears the served file so daemon shutdown / drain
			// stops exposing it on /stream. There's no ffmpeg child to kill.
			playerSessionRegistry.add(sess.SessionID, func() { streamSrv.ClearFile() })
			log.Printf("[stream %s] direct-play: %s", agent.ShortID(sess.SessionID), filepath.Base(filePath))
			// File is on disk → ready immediately. Tell the web so the player
			// attaches <video src> without burning its HEAD-probe retry budget.
			go func() {
				rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if err := agentClient.MarkSessionReady(rctx, sess.SessionID, nil); err != nil {
					log.Printf("[stream %s] mark-ready failed: %v", agent.ShortID(sess.SessionID), err)
				}
			}()
			return
		}

		tcRuntime := buildTranscodeRuntime(ctx, cfg)
		if tcRuntime.FFmpegPath == "" || tcRuntime.FFprobePath == "" {
			log.Printf("[hls %s] rejected: ffmpeg/ffprobe unavailable", agent.ShortID(sess.SessionID))
			return
		}

		// Remux path (hueco #3 / 3b): codecs are browser-native (h264/aac) but
		// the container isn't (mkv). ffmpeg `-c copy` → growing fMP4 served raw
		// over /stream — no video re-encode, no HLS. The web decided this from
		// scan metadata + version gate; we still need ffmpeg (copy uses it).
		if sess.PlayMethod == "remux" {
			tStart := time.Now()
			probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
			probe, perr := engine.ProbeFile(probeCtx, tcRuntime.FFprobePath, filePath)
			cancelProbe()
			if perr != nil {
				log.Printf("[stream %s] remux probe failed: %v", agent.ShortID(sess.SessionID), perr)
				return
			}
			tProbe := time.Now()
			remuxCtx, remuxCancel := context.WithCancel(ctx)
			src, serr := engine.NewRemuxSource(remuxCtx, filePath, probe, tcRuntime.FFmpegPath, sess.FileName)
			if serr != nil {
				remuxCancel()
				log.Printf("[stream %s] remux start failed: %v", agent.ShortID(sess.SessionID), serr)
				return
			}
			streamSrv.SetGrowingFile(src, sess.TaskID)
			// cancel stops the ffmpeg copy; SetGrowingFile/ClearFile also Close()
			// the source, so the temp file is always cleaned up.
			playerSessionRegistry.add(sess.SessionID, func() { remuxCancel(); streamSrv.ClearFile() })
			// Startup timing (TTFF diagnosis): probe = ffprobe on the source;
			// spawn = ffmpeg launch + tmp setup. First-fMP4-byte is logged by the
			// source itself; serveGrowing logs any client read that blocks waiting
			// for ffmpeg to catch up.
			log.Printf("[stream %s] remux (copy) → fMP4: %s [probe=%v spawn=%v]",
				agent.ShortID(sess.SessionID), filepath.Base(filePath),
				tProbe.Sub(tStart).Round(time.Millisecond), time.Since(tProbe).Round(time.Millisecond))
			go func() {
				rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if err := agentClient.MarkSessionReady(rctx, sess.SessionID, nil); err != nil {
					log.Printf("[stream %s] mark-ready failed: %v", agent.ShortID(sess.SessionID), err)
				}
			}()
			return
		}

		// Local-file HLS (the original path). StartHLSSession runs ffprobe
		// (15 s cap) inside startHLSPlayback's goroutine so the sync loop
		// returns immediately — browser HEAD probes have a 30 s retry budget
		// that absorbs the gap until the playlist registers.
		hlsCtx, hlsCancel := context.WithCancel(ctx)
		startHLSPlayback(engine.HLSSessionConfig{
			SessionID:         sess.SessionID,
			SourcePath:        filePath,
			FileName:          sess.FileName,
			Quality:           sess.Quality,
			AudioIndex:        sess.AudioIndex,
			BurnSubtitleIndex: sess.BurnSubtitleIndex,
			Transcode:         tcRuntime,
			Cache:             hlsCache,
		}, hlsCtx, hlsCancel)
	}

	// Periodic DHT node persistence (every 5 min)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				torrentDl.SaveDhtNodes()
			case <-ctx.Done():
				return
			}
		}
	}()

	// Periodic HLS session sweeper (every 5 min). Closes sessions whose last
	// segment fetch was over 30 min ago — kills the orphan ffmpeg + removes
	// the per-session tmpdir, so a tab that died mid-stream doesn't leak
	// disk space until daemon shutdown.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if n := streamSrv.HLS().SweepIdle(); n > 0 {
					log.Printf("[hls] swept %d idle session(s)", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Start auto-scan goroutine
	scanPaths := daemonCfg.ScanPaths
	if len(scanPaths) > 0 && cfg.Library.AutoScan {
		scanInterval := 24 * time.Hour
		if cfg.Library.ScanInterval != "" {
			if parsed, err := time.ParseDuration(cfg.Library.ScanInterval); err == nil && parsed > 0 {
				scanInterval = parsed
			}
		}
		go runAutoScan(ctx, cfg, scanInterval, agentClient, d.ScanNow, scanPaths)
	}

	// Start reporter only for stream task handling
	go reporter.Run(ctx)

	// Credential revoked mid-run (agent deleted from the dashboard): wipe the
	// stored key + agentId so a supervisor restart can't loop on a rejected
	// identity, then stop the daemon. Reconnecting needs a fresh `unarr login`.
	d.SyncClient().OnRevoked = func(err error) {
		reportAgentRevoked(cfg, err)
		cancel()
	}

	// Legacy bootstrap: if register hands back a per-machine key, persist it so
	// the next start authenticates with the bound agent key (one-time migration;
	// also stops the server re-minting on every restart).
	d.OnAgentKeyMinted = func(newKey string) {
		cfg.Auth.APIKey = newKey
		if serr := config.Save(cfg, config.FilePath()); serr != nil {
			log.Printf("[agent] could not persist per-machine key: %v", serr)
		} else {
			log.Printf("[agent] migrated to a per-machine agent key")
		}
	}

	// Start daemon (blocks — runs sync loop)
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx)
	}()

	// Start idle guard for the persistent stream server
	go startIdleGuard(ctx, streamSrv)

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Wait for signal or error
	select {
	case sig := <-sigCh:
		fmt.Printf("\n  Received %s, shutting down...\n", sig)
		cancelStreamContexts()
		cancelAllPlayerSessions()
		streamSrv.Shutdown(context.Background())

		// Drain active downloads BEFORE cancelling the daemon context. Shutdown
		// sets shuttingDown + cancels each task context itself, so interrupted
		// downloads keep their resume-store entry. Cancelling the shared ctx first
		// would make them look like genuine failures and wipe the entry → no resume.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		manager.Shutdown(shutdownCtx)

		cancel()
		d.Deregister()
		fmt.Println("  Daemon stopped.")
		return nil

	case err := <-errCh:
		cancelStreamContexts()
		cancelAllPlayerSessions()
		streamSrv.Shutdown(context.Background())
		cancel()
		// Registration was rejected because this agent's credential is revoked
		// (deleted from the dashboard). Wipe it and exit cleanly so the service
		// supervisor doesn't restart-loop against a 410; user must re-login.
		if agent.IsRevoked(err) {
			reportAgentRevoked(cfg, err)
			return nil
		}
		return err
	}
}

// reportAgentRevoked tells the user their agent was removed and wipes the
// stored credential (api key + agentId) so the next start requires a fresh
// `unarr login` (which mints a new per-machine key bound to a new agentId)
// instead of looping against a server that keeps rejecting the old identity.
func reportAgentRevoked(cfg config.Config, err error) {
	log.Printf("[agent] credential revoked by server (%v) — this machine was removed from your account", err)
	cfg.Auth.APIKey = ""
	cfg.Agent.ID = ""
	if serr := config.Save(cfg, config.FilePath()); serr != nil {
		log.Printf("[agent] could not clear stored credential: %v", serr)
	}
	fmt.Println()
	fmt.Println("  This agent was removed from your account.")
	fmt.Println("  Run `unarr login` on this machine to reconnect it.")
	fmt.Println()
}

// isAllowedStreamPath checks that filePath is within one of the directories
// the daemon is configured to manage. This defends against a compromised API
// server sending a path traversal payload (e.g. /etc/passwd) in StreamRequest.
// isAllowedStreamPath reports whether filePath is contained within one of the
// streamAllowedRoots returns the directory roots a stream / sidecar path is
// permitted under. Single source of truth so the raw /stream, HLS, and
// path-scoped (/thumbnail, /trickplay, /sub) handlers never disagree about what
// is reachable — a root added to one place but not the others would otherwise
// produce confusing partial failures (stream plays, scrubber frames 404).
func streamAllowedRoots(cfg config.Config) []string {
	return []string{cfg.Download.Dir, cfg.Library.ScanPath,
		cfg.Organize.MoviesDir, cfg.Organize.TVShowsDir}
}

// allowedDirs. filePath must already be cleaned (filepath.Clean) by the caller.
// This defends against a compromised API server sending a path traversal payload.
func isAllowedStreamPath(filePath string, allowedDirs ...string) bool {
	for _, dir := range allowedDirs {
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(dir), filePath)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

// relocateUnreachable tries to find a file the web asked us to stream under a
// path we can't serve (e.g. an old base path) by joining the longest suffix of
// that path onto each current allowed root and checking it exists. Returns the
// found absolute path or "".
//
// Conservative by design — it must never serve the WRONG file:
//   - Requires a tail of at least three segments (collection/season/file), so a
//     generic "Season 01/Episode.mkv" can't match a different show by accident.
//     Flat single-file-at-root layouts simply aren't self-healed here; the next
//     re-scan re-maps them instead.
//   - Re-checks containment AFTER resolving symlinks, so a symlink inside a root
//     pointing outside it can't be used to escape the allowed dirs (isAllowed‑
//     StreamPath alone is a lexical check that os.Stat would happily follow out).
func relocateUnreachable(filePath string, allowedRoots []string) string {
	segs := strings.Split(filepath.ToSlash(filePath), "/")
	// Longest tail first (most specific match wins). Stop before 3-segment tails
	// so a short, ambiguous suffix can't match the wrong file.
	for start := 0; start <= len(segs)-3; start++ {
		tail := filepath.Join(segs[start:]...)
		if tail == "" {
			continue
		}
		for _, root := range allowedRoots {
			if root == "" {
				continue
			}
			cand := filepath.Join(root, tail)
			if !isAllowedStreamPath(cand, root) {
				continue
			}
			fi, err := os.Stat(cand)
			if err != nil || fi.IsDir() {
				continue
			}
			// Re-validate containment against the symlink-resolved real paths so
			// a symlink under the root can't point the stream outside it.
			realCand, e1 := filepath.EvalSymlinks(cand)
			realRoot, e2 := filepath.EvalSymlinks(root)
			if e1 != nil || e2 != nil || !isAllowedStreamPath(realCand, realRoot) {
				continue
			}
			return cand
		}
	}
	return ""
}

func formatSpeedLog(bps int64) string {
	switch {
	case bps >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB/s", float64(bps)/(1024*1024*1024))
	case bps >= 1024*1024:
		return fmt.Sprintf("%.1f MB/s", float64(bps)/(1024*1024))
	case bps >= 1024:
		return fmt.Sprintf("%.0f KB/s", float64(bps)/1024)
	default:
		return fmt.Sprintf("%d B/s", bps)
	}
}

// runAutoScan runs a library scan + sync on a timer or on-demand via scanNow channel.
// It scans all provided paths and syncs each independently so stale-item cleanup
// is scoped to the correct directory prefix on the server.
// basePathChanged reports whether the library's scan root moved since the last
// saved cache — i.e. the previously-scanned root is no longer one of the current
// scan paths. Used to force a full (non-incremental) re-scan so the server can
// re-map paths by fingerprint and reap the old prefix.
func basePathChanged(existing *library.LibraryCache, scanPaths []string) bool {
	if existing == nil || len(existing.Items) == 0 || existing.Path == "" {
		return false
	}
	prev := filepath.Clean(existing.Path)
	for _, p := range scanPaths {
		if filepath.Clean(p) == prev {
			return false
		}
	}
	return true
}

func runAutoScan(ctx context.Context, cfg config.Config, interval time.Duration, ac *agent.Client, scanNow <-chan struct{}, scanPaths []string) {
	log.Printf("[auto-scan] enabled: every %s, paths: %v", interval, scanPaths)

	select {
	case <-time.After(30 * time.Second):
	case <-scanNow:
	case <-ctx.Done():
		return
	}

	doScan := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[auto-scan] panic recovered: %v", r)
			}
		}()
		log.Printf("[auto-scan] starting scan of %v", scanPaths)

		existing, _ := library.LoadCache()

		workers := cfg.Library.Workers
		if workers == 0 {
			workers = 8
		}

		// If the library base path changed (e.g. the agent moved from the host
		// binary to docker, remapping /mnt/nas/peliculas → /downloads, or the
		// user moved their media folder), force a FULL re-scan instead of an
		// incremental one. The fingerprint merge on the server then relocates
		// existing rows in place rather than duplicating, and per-agent cleanup
		// reaps the old prefix. See docs/plans/unarr-path-resilience.md.
		forceFull := basePathChanged(existing, scanPaths)
		if forceFull {
			log.Printf("[auto-scan] WARNING: library base path changed (was %q, now %v) — "+
				"running a FULL re-scan. This can take a while on large libraries; "+
				"playback and matches are preserved.", existing.Path, scanPaths)
		}

		scanOpts := library.ScanOptions{
			Workers:     workers,
			FFprobePath: cfg.Library.FFprobePath,
			Incremental: existing != nil && !forceFull,
		}

		// Resolve ffmpeg once for the sidecar prewarm (extracts text subs → WebVTT
		// and panel thumbnail frames → JPEG into the hidden ".unarr" cache so /sub
		// and /thumbnail are instant + huge remuxes work). Empty/err = prewarm is
		// skipped silently (on-demand extraction still runs).
		prewarmFFmpeg := ""
		if cfg.Library.CacheSubtitles || cfg.Library.CacheThumbnails || cfg.Library.Trickplay.Enabled {
			if ff, err := mediainfo.ResolveFFmpeg(cfg.Library.FFmpegPath); err == nil {
				prewarmFFmpeg = ff
			} else {
				log.Printf("[auto-scan] sidecar prewarm disabled: ffmpeg unavailable: %v", err)
			}
		}

		// Scan each path independently and sync per path so the server can
		// scope stale-item deletion to the correct directory prefix.
		const batchSize = 100
		totalSynced := 0
		var mergedItems []library.LibraryItem

		for _, scanPath := range scanPaths {
			cache, err := library.Scan(ctx, scanPath, existing, scanOpts)
			if err != nil {
				log.Printf("[auto-scan] scan failed for %s: %v", scanPath, err)
				continue
			}
			mergedItems = append(mergedItems, cache.Items...)

			if prewarmFFmpeg != "" {
				library.PrewarmSidecars(ctx, cache, library.PrewarmOptions{
					FFmpegPath:           prewarmFFmpeg,
					CacheSubtitles:       cfg.Library.CacheSubtitles,
					CacheThumbnails:      cfg.Library.CacheThumbnails,
					Workers:              2,
					Trickplay:            cfg.Library.Trickplay.Enabled,
					TrickplayIntervalSec: cfg.Library.Trickplay.IntervalSeconds(),
					TrickplayWidth:       cfg.Library.Trickplay.Width,
					MaxLoadRatio:         cfg.Library.PrewarmMaxLoadRatio,
				})
			}

			items := library.BuildSyncItems(cache)
			if len(items) == 0 {
				log.Printf("[auto-scan] no items under %s", scanPath)
				continue
			}

			syncStartedAt := time.Now().UTC().Format(time.RFC3339)
			for i := 0; i < len(items); i += batchSize {
				end := i + batchSize
				if end > len(items) {
					end = len(items)
				}
				isLast := end >= len(items)

				_, err := ac.SyncLibrary(ctx, agent.LibrarySyncRequest{
					Items:         items[i:end],
					ScanPath:      scanPath,
					AgentID:       cfg.Agent.ID,
					IsLastBatch:   isLast,
					SyncStartedAt: syncStartedAt,
				})
				if err != nil {
					log.Printf("[auto-scan] sync failed for %s: %v", scanPath, err)
					break
				}
			}
			totalSynced += len(items)
		}

		// Save merged cache for incremental scanning next time.
		if len(mergedItems) > 0 {
			mergedCache := &library.LibraryCache{
				ScannedAt: time.Now().UTC().Format(time.RFC3339),
				Path:      scanPaths[0],
				Items:     mergedItems,
			}
			if err := library.SaveCache(mergedCache); err != nil {
				log.Printf("[auto-scan] save cache failed: %v", err)
			}
		}

		log.Printf("[auto-scan] synced %d items across %d path(s)", totalSynced, len(scanPaths))
	}

	doScan()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			doScan()
		case <-scanNow:
			log.Printf("[auto-scan] on-demand scan triggered")
			ticker.Reset(interval)
			doScan()
		case <-ctx.Done():
			return
		}
	}
}

// superviseFunnel keeps a CloudFlare Quick Tunnel up across cloudflared
// crashes and CF's ~6h tunnel rotation. On a clean exit (cancellation) it
// returns; on a crash it clears the reported URL and respawns with an
// exponential backoff so we don't hammer cloudflared into a tight loop when
// it can't reach the CF edge.
func superviseFunnel(ctx context.Context, d *agent.Daemon, port int) {
	backoff := 2 * time.Second
	const maxBackoff = 5 * time.Minute
	for ctx.Err() == nil {
		t, err := funnel.Start(ctx, funnel.Config{Port: port})
		if err != nil {
			log.Printf("[funnel] could not start CloudFlare tunnel (%v) — retrying in %s", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		log.Printf("[funnel] cloudflared started, waiting for public URL...")
		go func() {
			url, werr := t.WaitURL(45 * time.Second)
			if werr != nil {
				log.Printf("[funnel] cloudflared did not emit a URL (%v)", werr)
				return
			}
			log.Printf("[funnel] public URL: %s", url)
			d.SetFunnelURL(url)
		}()
		// Block until cloudflared exits (CF rotation, crash, or shutdown).
		exitErr := <-t.Done()
		_ = t.Close()
		d.SetFunnelURL("")
		if ctx.Err() != nil {
			return
		}
		if exitErr != nil {
			log.Printf("[funnel] cloudflared exited: %v — restarting in %s", exitErr, backoff)
		} else {
			log.Printf("[funnel] cloudflared exited cleanly — restarting in %s", backoff)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// mirrorCORSOrigins fetches /api/mirrors from the configured primary (+ extra
// mirror candidates + static IPFS fallback) and returns the discovered URLs as
// Origin strings. Best-effort: any failure logs a warning and returns an empty
// slice; the static defaultCORSAllowedOrigins in validate.go covers the known
// mirrors (.com / .to / built-in onion) so the daemon still accepts the
// official surfaces when this call fails.
//
// Bounded to a short timeout so a slow /api/mirrors response can't delay
// daemon startup — every second here is a second the user can't play.
func mirrorCORSOrigins(parent context.Context, cfg config.Config, userAgent string) []string {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	candidates := append([]string{cfg.Auth.APIURL}, cfg.Auth.Mirrors...)
	resp, err := agent.FetchMirrorsWithFallback(ctx, candidates, userAgent)
	if err != nil {
		log.Printf("[cors] mirror discovery failed (%v) — using static allowlist only", err)
		return nil
	}

	seen := make(map[string]struct{})
	out := make([]string, 0, len(resp.Mirrors))
	add := func(rawURL string) {
		if rawURL == "" {
			return
		}
		origin := strings.TrimRight(rawURL, "/")
		if _, dup := seen[origin]; dup {
			return
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	for _, m := range resp.Mirrors {
		add(m.URL)
	}
	if resp.Tor != nil {
		add(resp.Tor.URL)
	}
	if len(out) > 0 {
		log.Printf("[cors] merged %d mirror origins from /api/mirrors", len(out))
	}
	return out
}

// watchSessionReady polls HLSSession.ReadyCount until the first segment +
// init.mp4 are on disk, then POSTs /api/internal/agent/session-ready so
// the web side flips streaming_session.ready_at — which its SSE endpoint
// pushes to subscribed players. Cache-HIT sessions are ready the moment
// StartHLSSession returns and POST immediately.
//
// Bounded by a 60 s deadline so a permanently stuck encoder doesn't keep
// a goroutine alive forever; if seg-0 never lands the player falls back
// to its existing HEAD-probe retry path anyway.
func watchSessionReady(ctx context.Context, client *agent.Client, hsess *engine.HLSSession, sessionID string) {
	deadline := time.Now().Add(60 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	readyPosted := false
	postReady := func(health *agent.SessionHealth) {
		// Parent ctx so a session cancel mid-POST (user closed tab, daemon
		// shutdown) tears down the in-flight webhook instead of blocking the
		// goroutine for up to 10 s on a now-orphan call.
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := client.MarkSessionReady(rctx, sessionID, health); err != nil {
			log.Printf("[hls %s] mark-ready failed: %v", agent.ShortID(sessionID), err)
		}
		cancel()
	}
	for {
		// Session torn down through a path that didn't cancel ctx (registry
		// replace, idle sweep, internal kill). Bail before polling further —
		// without this check the watcher could keep alive for up to 60 s on
		// a dead HLSSession that's never going to become ready.
		if hsess.IsClosed() {
			return
		}
		// Phase 1: cache HIT or seg-0 ready → flip the "Preparando…" UI now.
		if !readyPosted && (hsess.FromCache() || hsess.ReadyCount() >= 1) {
			postReady(nil)
			readyPosted = true
			// Cache replay has no live encode → no telemetry to report, done.
			if hsess.FromCache() {
				return
			}
		}
		// Phase 2 (F3): once enough -stats samples accumulated (encoder past
		// its cold ramp), report ONE live-health snapshot so the player can
		// name a too-slow transcode in ~4s instead of inferring it from stalls.
		// >=4 samples ≈ 2s of encoding past seg-0; the EWMA has settled by then.
		if readyPosted {
			if st := hsess.GetTranscodeStats(); st.Samples >= 4 {
				postReady(classifyAgentHealth(st))
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			if !readyPosted {
				log.Printf("[hls %s] mark-ready: timeout waiting for seg-0", agent.ShortID(sessionID))
				return
			}
			// Ready but never got stable telemetry — report whatever we have so
			// the player isn't left without a verdict (better partial than none).
			if st := hsess.GetTranscodeStats(); st.Samples > 0 {
				postReady(classifyAgentHealth(st))
			}
			return
		}
	}
}

// Realtime-ratio cutoffs for classifyAgentHealth. This is a cross-repo contract
// with the web bottleneck classifier (src/lib/stream/bottleneck-classifier.ts):
//   - ≥ realtimeFloor      → "ok" (encoder keeps up)
//   - [strugglingFloor,..) → "marginal" (barely)
//   - < strugglingFloor    → "struggling" (can't) — the web fast-path commits
//     the honest overlay + pauses on this WITHOUT waiting for a stall, so the
//     floor is intentionally conservative (the web uses a looser 0.85 only once
//     a stall has already corroborated the slowdown).
const (
	agentRealtimeFloor   = 0.95
	agentStrugglingFloor = 0.75
)

// classifyAgentHealth turns a live ffmpeg telemetry snapshot into the health
// report the web side consumes (F3). The ×realtime speed is the load-bearing
// signal: < 1.0 means the encode can't keep up with playback. An input-bound
// hint (source read error) reclassifies the cause as the link, not the encoder.
func classifyAgentHealth(st engine.TranscodeStats) *agent.SessionHealth {
	ratio := st.SpeedX
	var health, reason string
	switch {
	case st.InputBound && ratio < agentRealtimeFloor:
		health, reason = "struggling", "input_bound"
	case ratio >= agentRealtimeFloor:
		health, reason = "ok", "realtime"
	case ratio >= agentStrugglingFloor:
		health, reason = "marginal", "transcode"
	default:
		health, reason = "struggling", "transcode"
	}
	return &agent.SessionHealth{Health: health, RealtimeRatio: ratio, Reason: reason}
}
