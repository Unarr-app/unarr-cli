package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/metrics"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
)

// runStatusSessions is `unarr status --sessions`: the same snapshot the web
// reads from GET /sessions, rendered for a terminal (or raw with --json).
func runStatusSessions(ctx context.Context, asJSON bool) error {
	ctx, cancel := withDownloadsTimeout(ctx)
	defer cancel()

	client, err := controlClient()
	if err != nil {
		return err
	}
	snap, err := client.Sessions(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(snap)
	}
	printSessions(os.Stdout, snap)
	return nil
}

func printSessions(w io.Writer, snap metrics.Snapshot) {
	bold := color.New(color.Bold)
	dim := color.New(color.FgHiBlack)

	fmt.Fprintln(w)
	if len(snap.Sessions) == 0 {
		dim.Fprintln(w, "  No active sessions.")
	} else {
		bold.Fprintf(w, "  %-10s %-9s %-12s %-9s %10s %10s %8s %6s  %s\n",
			"ID", "KIND", "STATE", "HEALTH", "RATE", "BYTES", "ACTIVE", "STALLS", "TITLE")
		for _, s := range snap.Sessions {
			fmt.Fprintf(w, "  %-10s %-9s %-12s %-9s %10s %10s %8s %6d  %s\n",
				agent.ShortID(s.ID), s.Kind, s.State, healthLabel(s.Health), sessionRate(s),
				humanize.Bytes(uint64(s.Bytes)), (time.Duration(s.ActiveSec) * time.Second).String(),
				s.Stall.Count, truncateTitle(s.Title, 40))
		}
	}

	sum := snap.Summary
	fmt.Fprintln(w)
	dim.Fprintf(w, "  VPN: %s", vpnLabel(sum.VPN))
	if len(sum.DownloadsByState) > 0 {
		dim.Fprintf(w, "   downloads: %d known", countAll(sum.DownloadsByState))
	}
	dim.Fprintf(w, "   agent %s, up %s\n\n", snap.Agent.Version, (time.Duration(snap.Agent.UptimeSec) * time.Second).String())
}

// sessionRate prefers the encoder's ×realtime when the session has one — bytes/s
// means little for a transcode.
func sessionRate(s metrics.Session) string {
	switch {
	case s.SpeedX != nil:
		return fmt.Sprintf("%.2fx", *s.SpeedX)
	case s.RateBps > 0:
		return humanize.Bytes(uint64(s.RateBps)) + "/s"
	}
	return "—"
}

func healthLabel(h metrics.Health) string {
	switch h {
	case metrics.HealthStalled:
		return color.New(color.FgRed).Sprint(h)
	case metrics.HealthDegraded:
		return color.New(color.FgYellow).Sprint(h)
	default:
		return color.New(color.FgGreen).Sprint(h)
	}
}

func vpnLabel(v metrics.VPN) string {
	switch {
	case !v.Enabled && !v.Required:
		return "off"
	case v.Up:
		return "up"
	case v.Required:
		return "DOWN (kill-switch blocking P2P)"
	default:
		return "down"
	}
}

func countAll(m map[string]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}
