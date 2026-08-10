package doctor

import (
	"fmt"
	"io"

	"github.com/fatih/color"
)

// TextRenderer reproduces the historical `unarr doctor` console output. It is
// streaming by contract: Start prints the banner, OnCheck prints one line per
// check as it lands (group headers appear lazily, when the group changes), and
// Finish prints the summary. Everything goes through a single writer so the
// colored and plain fragments of a line can't interleave out of order — the
// pre-refactor code split them between color.Output and os.Stdout, which only
// happened to work because they are the same fd on POSIX.
type TextRenderer struct {
	w      io.Writer
	bold   *color.Color
	green  *color.Color
	red    *color.Color
	yellow *color.Color
	dim    *color.Color

	// ShowFixTip prints the trailing `doctor --fix` hint when the run had
	// failures. Off during --fix itself, which is about to do the repairs.
	ShowFixTip bool

	lastGroup string
}

// NewTextRenderer builds a renderer writing to w. Callers normally pass
// color.Output so Windows gets ANSI translation.
func NewTextRenderer(w io.Writer) *TextRenderer {
	return &TextRenderer{
		w:      w,
		bold:   color.New(color.Bold),
		green:  color.New(color.FgGreen),
		red:    color.New(color.FgRed),
		yellow: color.New(color.FgYellow),
		dim:    color.New(color.Faint),
	}
}

func (r *TextRenderer) Start() {
	fmt.Fprintln(r.w)
	r.bold.Fprintln(r.w, "  unarr Diagnostics")
	fmt.Fprintln(r.w)
}

// OnCheck is the per-check callback handed to Run.
func (r *TextRenderer) OnCheck(c Check) {
	if c.Group != r.lastGroup {
		if r.lastGroup != "" {
			fmt.Fprintln(r.w)
		}
		r.bold.Fprintln(r.w, "  "+c.Group)
		r.lastGroup = c.Group
	}

	// The pass styling is also the fallback: a Status this renderer has never
	// heard of prints as a plain check rather than panicking on a nil colour.
	// StatusPass is still spelled out so `exhaustive` keeps guarding the switch —
	// a new status added to check.go must be decided here, not defaulted.
	col, symbol := r.green, "+"
	switch c.Status {
	case StatusPass:
	case StatusFail:
		col, symbol = r.red, "x"
	case StatusWarn:
		col, symbol = r.yellow, "!"
	}
	col.Fprintf(r.w, "  %s %s", symbol, c.Name)
	if c.Message != "" {
		fmt.Fprintf(r.w, " — %s", c.Message)
	}
	fmt.Fprintln(r.w)
}

func (r *TextRenderer) Finish(rep Report) {
	fmt.Fprintln(r.w)
	switch {
	case rep.Failed == 0 && rep.Warned == 0:
		r.green.Fprintln(r.w, "  All checks passed!")
	case rep.Failed == 0:
		r.yellow.Fprintf(r.w, "  %d passed, %d warnings\n", rep.Passed, rep.Warned)
	default:
		r.red.Fprintf(r.w, "  %d passed, %d failed, %d warnings\n", rep.Passed, rep.Failed, rep.Warned)
	}
	fmt.Fprintln(r.w)

	if r.ShowFixTip && rep.Failed > 0 {
		// Kept at 60 columns or under: this is the widest line the renderer
		// emits on its own, and a 60-col terminal (a split pane) wrapping the
		// closing hint is the ugliest place to lose a column. Pinned by
		// TestRenderFrameFitsSixtyColumns.
		r.dim.Fprintln(r.w, "  Tip: `unarr doctor --fix` repairs common issues.")
		fmt.Fprintln(r.w)
	}
}
