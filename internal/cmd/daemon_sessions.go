package cmd

import (
	"context"
	"log"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/engine"
	"github.com/Unarr-app/unarr-cli/internal/metrics"
	"github.com/Unarr-app/unarr-cli/internal/remotefs"
	"github.com/Unarr-app/unarr-cli/internal/vpn"
)

// sessionSampleInterval is the stall monitor's tick. The grace window
// (metrics.GraceDefault) is measured in wall time, so this only sets how late a
// stall can be noticed.
const sessionSampleInterval = 5 * time.Second

// hlsIdleAfter is how long without a viewer request before an HLS session is
// reported "idle" (a player fetches a segment every few seconds).
const hlsIdleAfter = 30 * time.Second

// sessionDeps are the daemon pieces the unified session view reads. Nothing is
// copied: every source reads the owner's registry on demand.
type sessionDeps struct {
	version    string
	cfg        config.Config
	manager    *engine.Manager
	streamSrv  *engine.StreamServer
	vpnTunnel  *vpn.Tunnel
	controller *taskController
}

// newSessionHub wires the daemon's subsystems into one metrics.Hub.
func newSessionHub(d sessionDeps) *metrics.Hub {
	hub := metrics.NewHub(d.version, func(ev metrics.StallEvent) {
		log.Printf("[sessions] stall: %s %s (%s)", ev.Kind, agent.ShortID(ev.ID), ev.Reason)
	})
	hub.AddSource(func() []metrics.Reading { return downloadReadings(d.manager) })
	hub.AddSource(func() []metrics.Reading { return hlsReadings(d.streamSrv.HLS(), time.Now()) })
	hub.AddSource(func() []metrics.Reading { return streamReadings(d.streamSrv, d.manager) })
	hub.AddSource(mountReadings)
	hub.SetBytesCounter(metrics.KindMount, func() int64 { _, b := remotefs.ReadStats(); return b })
	hub.SetVPN(func() metrics.VPN {
		vc := d.cfg.Download.VPN
		return metrics.VPN{
			Enabled:  vc.Enabled || vc.ConfigFile != "",
			Required: vc.Required,
			Up:       d.vpnTunnel.Healthy(), // nil-safe: a missing tunnel is down
		}
	})
	hub.SetDownloadStates(func() map[string]int {
		counts := map[string]int{}
		for _, t := range d.controller.List() {
			counts[t.State]++
		}
		return counts
	})
	return hub
}

// startSessionView starts the hub's sampler (it feeds the stall log whether or
// not anybody scrapes) and — only when [metrics] enabled — the Prometheus
// endpoint. Disabled metrics bind no port and start no /metrics goroutine.
// Returns the endpoint, nil when it is off or could not start.
func startSessionView(ctx context.Context, hub *metrics.Hub, mc config.MetricsConfig, controlToken string) *metrics.Server {
	go hub.Run(ctx, sessionSampleInterval)
	if !mc.Enabled {
		return nil
	}
	if err := mc.Validate(); err != nil {
		log.Printf("[metrics] not started: %v", err)
		return nil
	}
	token := mc.Token
	if token == "" {
		token = controlToken // loopback only, enforced by Validate
	}
	if token == "" {
		log.Printf("[metrics] not started: no control-plane token to protect the endpoint")
		return nil
	}
	srv, err := metrics.Listen(ctx, mc.Address(), token, hub)
	if err != nil {
		log.Printf("[metrics] not started: %v", err)
	}
	return srv
}

func downloadReadings(m *engine.Manager) []metrics.Reading {
	tasks := m.ActiveTasks()
	out := make([]metrics.Reading, 0, len(tasks))
	for _, t := range tasks {
		u := t.ToStatusUpdate()
		status := t.GetStatus()
		progress := u.Progress
		out = append(out, metrics.Reading{
			Session: metrics.Session{
				ID: t.ID, Kind: metrics.KindDownload, Title: t.Title,
				State: string(status), Bytes: u.DownloadedBytes, RateBps: u.SpeedBps,
				Progress: &progress,
			},
			RateKnown:  true,
			ExpectFlow: status == engine.StatusDownloading,
			Reason:     "no_data",
		})
	}
	return out
}

func hlsReadings(reg *engine.HLSSessionRegistry, now time.Time) []metrics.Reading {
	infos := reg.Infos()
	out := make([]metrics.Reading, 0, len(infos))
	for _, in := range infos {
		out = append(out, hlsReading(in, now))
	}
	return out
}

// hlsReading maps one HLS session. Impairment reuses classifyAgentHealth — the
// verdict already shared with the web — so "stalled" here and "struggling"
// there can never disagree: only a "struggling" encoder counts as impaired.
func hlsReading(in engine.HLSInfo, now time.Time) metrics.Reading {
	state := "active"
	if now.Sub(in.LastTouch) > hlsIdleAfter {
		state = "idle"
	}
	r := metrics.Reading{Session: metrics.Session{
		ID: in.ID, Kind: metrics.KindHLS, Title: in.FileName, State: state,
		Bytes: in.ServedBytes, StartedAt: in.StartedAt,
	}}
	switch {
	case in.Failed:
		r.Impaired, r.Reason = true, "encoder_failed"
	case state == "active" && in.Stats.Samples > 0 && !in.Stats.FromCache:
		speed := in.Stats.SpeedX
		r.SpeedX = &speed
		if h := classifyAgentHealth(in.Stats); h.Health == "struggling" {
			r.Impaired, r.Reason = true, h.Reason
		}
	}
	return r
}

func streamReadings(srv *engine.StreamServer, m *engine.Manager) []metrics.Reading {
	if srv.ActiveReaders() == 0 {
		return nil
	}
	id := srv.CurrentTaskID()
	if id == "" {
		return nil
	}
	title := ""
	if t := m.GetTask(id); t != nil {
		title = t.Title
	}
	return []metrics.Reading{{Session: metrics.Session{
		ID: id, Kind: metrics.KindStream, Title: title, State: "active",
	}}}
}

// mountReadings reports the WebDAV mount as ONE session while it is serving a
// read; an idle mount is not a session. A read in flight that moves no bytes is
// what a stalled NNTP/remote fetch looks like from here. The byte counter is the
// process total: the hub turns it into the session's own bytes.
func mountReadings() []metrics.Reading {
	active, bytes := remotefs.ReadStats()
	if active == 0 {
		return nil
	}
	return []metrics.Reading{{
		Session:         metrics.Session{ID: "mount", Kind: metrics.KindMount, Title: "WebDAV mount", State: "active", Bytes: bytes},
		ExpectFlow:      true,
		Reason:          "no_data",
		CumulativeBytes: true,
	}}
}
