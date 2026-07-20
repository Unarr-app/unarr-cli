package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// trayState is what the icon communicates at a glance.
type trayState int

const (
	stateUnknown trayState = iota
	stateRunning
	stateDownloading
	statePaused
	stateStopped
	stateCrashed
	// stateFailed: a control the user asked for (start/stop/restart) failed.
	// Distinct from stateCrashed — same red badge, because both are errors the
	// user must see, but a failed control must never be mistaken for a crash
	// and reported as one.
	stateFailed
	// stateBlocked: the daemon is alive but the server will not let it work —
	// a rejected credential, an exhausted plan. It OUTRANKS running: a process
	// with a live PID that cannot accept a single download is the state this
	// tray used to render as a healthy green agent, which told the user nothing
	// was wrong while nothing worked.
	stateBlocked
)

func (s trayState) label() string {
	switch s {
	case stateRunning:
		return "running"
	case stateDownloading:
		return "downloading"
	case statePaused:
		return "paused"
	case stateStopped:
		return "stopped"
	case stateCrashed:
		return "crashed"
	case stateFailed:
		return "failed"
	case stateBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// displayState maps daemon status + the tray's own pause marker to the icon
// state. Pause and stop are the same daemon operation (clean stop); the marker
// is what distinguishes "I paused it from the tray" from "it is not running".
// Running with ≥1 active task is its OWN state so the icon carries an activity
// badge — the running↔downloading boundary is the 0↔>0 task transition, which
// is exactly when applyState (transition-only) swaps the icon.
// failed reports that the last control the user asked for did not work; it
// outranks paused/stopped because "it is not running" is precisely the part the
// user already knows — why is the part they are missing.
func displayState(s agentStatus, paused, failed, blocked bool) trayState {
	switch {
	case blocked && s.running:
		// Checked before "running" on purpose: the daemon IS running, which is
		// exactly why this needs to outrank it.
		//
		// But ONLY while it is running. A record left behind by a daemon that
		// has since stopped is stale by definition, and letting it win would
		// disable every control — including Resume, the only one that starts the
		// agent — leaving the user with no way out but deleting a file by hand.
		// Starting the agent is exactly the right move there: it either
		// re-registers and clears the record, or parks again and re-states the
		// problem with fresh information.
		return stateBlocked
	case s.running && s.tasks > 0:
		return stateDownloading
	case s.running:
		return stateRunning
	case s.crashed:
		return stateCrashed
	case failed:
		return stateFailed
	case paused:
		return statePaused
	default:
		return stateStopped
	}
}

var (
	amber = color.NRGBA{R: 0xF5, G: 0xA6, B: 0x23, A: 0xFF}
	red   = color.NRGBA{R: 0xE0, G: 0x33, B: 0x2C, A: 0xFF}
	// green reads on the COLORED (amber) logo, unlike amber-on-amber — it marks
	// the running icon while a download is active.
	green = color.NRGBA{R: 0x2E, G: 0xC4, B: 0x66, A: 0xFF}
)

// buildStateIcons derives the per-state tray icons from the embedded logo at
// startup. One visual system: colored logo = running; grayscale base = not
// running, with the badge color telling why — amber dot = paused (tray-
// initiated), red dot = crashed, no dot = plain stopped. Badges sit on the
// gray base (never on the colored logo: the logo is amber, an amber badge on
// it is invisible). Runtime generation keeps a single source asset — variants
// can never drift from the logo. Any failure falls back to the plain logo.
func buildStateIcons(base []byte) map[trayState][]byte {
	fallback := map[trayState][]byte{
		stateRunning: base, stateDownloading: base, statePaused: base,
		stateStopped: base, stateCrashed: base, stateFailed: base,
		stateBlocked: base,
	}
	src, err := png.Decode(bytes.NewReader(base))
	if err != nil {
		return fallback
	}
	gray := dimGray(src)
	return map[trayState][]byte{
		stateRunning: base,
		// Downloading: green badge on the COLORED logo (amber-on-amber would be
		// invisible, hence green, unlike the not-running badges on gray).
		stateDownloading: encodeOr(base, badge(src, green)),
		statePaused:      encodeOr(base, badge(gray, amber)),
		stateStopped:     encodeOr(base, gray),
		stateCrashed:     encodeOr(base, badge(gray, red)),
		// A failed control is an error the user must notice, so it gets the
		// same red badge as a crash.
		stateFailed: encodeOr(base, badge(gray, red)),
		// Blocked waits on the user, like a failed control does, so it wears the
		// same red badge on gray — the daemon may be alive, but nothing it can
		// do matters until the user acts.
		stateBlocked: encodeOr(base, badge(gray, red)),
	}
}

func encodeOr(fallback []byte, img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return fallback
	}
	return buf.Bytes()
}

// badge overlays a filled colored dot on the bottom-right corner (~30% of the
// icon size, with a thin transparent cut-out ring so it reads on any theme).
func badge(src image.Image, c color.NRGBA) image.Image {
	b := src.Bounds()
	dst := image.NewNRGBA(b)
	draw.Draw(dst, b, src, b.Min, draw.Src)

	w := b.Dx()
	r := w * 30 / 200 // dot radius ≈ 15% of width → 30% diameter
	if r < 3 {
		r = 3
	}
	ring := r + maxInt(1, r/4)
	cx := b.Max.X - r - 1
	cy := b.Max.Y - r - 1

	for y := cy - ring; y <= cy+ring; y++ {
		for x := cx - ring; x <= cx+ring; x++ {
			if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
				continue
			}
			dx, dy := x-cx, y-cy
			d2 := dx*dx + dy*dy
			switch {
			case d2 <= r*r:
				dst.SetNRGBA(x, y, c)
			case d2 <= ring*ring:
				dst.SetNRGBA(x, y, color.NRGBA{}) // cut-out ring
			}
		}
	}
	return dst
}

// dimGray converts the icon to a dimmed grayscale so "stopped" is visibly
// inactive without shouting.
func dimGray(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, a := src.At(x, y).RGBA()
			// Rec. 601 luma, 16-bit → 8-bit
			l := uint8((299*r + 587*g + 114*bb) / 1000 >> 8)
			dst.SetNRGBA(x, y, color.NRGBA{R: l, G: l, B: l, A: uint8(a >> 8 * 55 / 100)})
		}
	}
	return dst
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
