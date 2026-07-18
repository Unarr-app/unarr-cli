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
	statePaused
	stateStopped
	stateCrashed
)

func (s trayState) label() string {
	switch s {
	case stateRunning:
		return "running"
	case statePaused:
		return "paused"
	case stateStopped:
		return "stopped"
	case stateCrashed:
		return "crashed"
	default:
		return "unknown"
	}
}

// displayState maps daemon status + the tray's own pause marker to the icon
// state. Pause and stop are the same daemon operation (clean stop); the marker
// is what distinguishes "I paused it from the tray" from "it is not running".
func displayState(s agentStatus, paused bool) trayState {
	switch {
	case s.running:
		return stateRunning
	case s.crashed:
		return stateCrashed
	case paused:
		return statePaused
	default:
		return stateStopped
	}
}

var (
	amber = color.NRGBA{R: 0xF5, G: 0xA6, B: 0x23, A: 0xFF}
	red   = color.NRGBA{R: 0xE0, G: 0x33, B: 0x2C, A: 0xFF}
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
		stateRunning: base, statePaused: base, stateStopped: base, stateCrashed: base,
	}
	src, err := png.Decode(bytes.NewReader(base))
	if err != nil {
		return fallback
	}
	gray := dimGray(src)
	return map[trayState][]byte{
		stateRunning: base,
		statePaused:  encodeOr(base, badge(gray, amber)),
		stateStopped: encodeOr(base, gray),
		stateCrashed: encodeOr(base, badge(gray, red)),
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
