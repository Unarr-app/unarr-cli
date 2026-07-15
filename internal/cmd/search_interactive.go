package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	tc "github.com/torrentclaw/go-client"

	"github.com/Unarr-app/unarr-cli/internal/ui"
)

// runSearchInteractive drives the TTY picker: choose a title, choose a release,
// then act on it (stream / copy magnet / show info hash). When autoStream is
// true the action menu is skipped and the chosen release streams immediately.
//
// It loops so "Back" (the action-menu item) returns to the release list. In
// huh v1 the only key that leaves a prompt is Ctrl-C (huh.ErrUserAborted),
// which cancels cleanly without an error at every level.
func runSearchInteractive(resp *tc.SearchResponse, autoStream bool) error {
	if resp == nil || len(resp.Results) == 0 {
		color.New(color.FgYellow).Println("No results found.")
		return nil
	}

	for {
		result, ok, err := pickResult(resp.Results)
		if err != nil {
			return err
		}
		if !ok {
			return nil // cancelled at the top level
		}

		if len(result.Torrents) == 0 {
			color.New(color.FgHiBlack).Printf("  No releases available for %s.\n", result.Title)
			if len(resp.Results) == 1 {
				return nil
			}
			continue // back to the title list
		}

		tor, ok, err := pickTorrent(result)
		if err != nil {
			return err
		}
		if !ok {
			if len(resp.Results) == 1 {
				return nil // single title — nothing to go back to, so exit
			}
			continue // back to the title list
		}

		if autoStream {
			return streamTorrent(tor)
		}

		action, err := pickAction(tor)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil // Ctrl-C exits cleanly; the "Back" item handles going back
			}
			return err
		}

		switch action {
		case "stream":
			return streamTorrent(tor)
		case "copy":
			copyMagnet(tor)
			return nil
		case "hash":
			printHash(tor)
			return nil
		case "back":
			continue
		default: // "quit"
			return nil
		}
	}
}

// pickResult selects a title. Returns ok=false when the user cancels. A single
// result skips the prompt.
func pickResult(results []tc.SearchResult) (tc.SearchResult, bool, error) {
	if len(results) == 1 {
		return results[0], true, nil
	}

	opts := make([]huh.Option[int], 0, len(results))
	for i, r := range results {
		opts = append(opts, huh.NewOption(resultLabel(r), i))
	}

	var idx int
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[int]().
				Title("Select a title").
				Options(opts...).
				Value(&idx),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return tc.SearchResult{}, false, nil
		}
		return tc.SearchResult{}, false, err
	}
	return results[idx], true, nil
}

// pickTorrent selects a release within a title. Returns ok=false when the user
// aborts (Ctrl-C). A single release skips the prompt.
func pickTorrent(r tc.SearchResult) (tc.TorrentInfo, bool, error) {
	if len(r.Torrents) == 1 {
		return r.Torrents[0], true, nil
	}

	opts := make([]huh.Option[int], 0, len(r.Torrents))
	for i, t := range r.Torrents {
		opts = append(opts, huh.NewOption(torrentLabel(t), i))
	}

	var idx int
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[int]().
				Title(fmt.Sprintf("Select a release — %s (%s)", r.Title, ui.FormatYear(r.Year))).
				Options(opts...).
				Value(&idx),
		),
	).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return tc.TorrentInfo{}, false, nil
		}
		return tc.TorrentInfo{}, false, err
	}
	return r.Torrents[idx], true, nil
}

// pickAction shows what to do with the chosen release.
func pickAction(t tc.TorrentInfo) (string, error) {
	var action string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("%s — %s", ui.StringOrDash(t.Quality), truncate(t.RawTitle, 60))).
				Options(
					huh.NewOption("Stream now       — play in mpv or VLC while it loads", "stream"),
					huh.NewOption("Copy magnet      — to clipboard", "copy"),
					huh.NewOption("Show info hash   — print hash + magnet", "hash"),
					huh.NewOption("Back             — pick another release", "back"),
					huh.NewOption("Quit", "quit"),
				).
				Value(&action),
		),
	).Run()
	if err != nil {
		return "", err
	}
	return action, nil
}

func streamTorrent(t tc.TorrentInfo) error {
	m, ok := magnetOf(t)
	if !ok {
		return errNoPlayable
	}
	return runStream(m, 0, false, "")
}

func copyMagnet(t tc.TorrentInfo) {
	m, ok := magnetOf(t)
	if !ok {
		color.New(color.FgYellow).Println("  " + noPlayableHint)
		return
	}
	if err := clipboard.WriteAll(m); err != nil {
		// Headless Linux without xclip/xsel, or no clipboard access — print it so
		// the magnet is never lost.
		fmt.Println(m)
		color.New(color.FgYellow).Println("  Clipboard unavailable — magnet printed above.")
		return
	}
	color.New(color.FgGreen).Println("  Magnet copied to clipboard.")
}

func printHash(t tc.TorrentInfo) {
	m, ok := magnetOf(t)
	if !ok {
		color.New(color.FgYellow).Println("  " + noPlayableHint)
		return
	}
	bold := color.New(color.Bold)
	bold.Printf("  info hash: ")
	fmt.Println(t.InfoHash)
	bold.Printf("  magnet:    ")
	fmt.Println(m)
}

// noPlayableHint explains why a release has no actionable magnet: the public
// catalog only returns download links (info hash + magnet) to authenticated API
// keys, so an anonymous or invalid key gets browse-only metadata.
const noPlayableHint = "No magnet for this release. The catalog returns download links only to a valid API key — run `unarr login` (or `unarr init` to set a key), then try again."

var errNoPlayable = fmt.Errorf("%s", noPlayableHint)

// magnetOf returns the release's magnet URI and whether one is available. It
// synthesizes a magnet from the info hash when the source didn't provide a full
// magnet link; either form is a valid argument to `unarr stream`. ok is false
// when the response carried neither a magnet nor a 40-char info hash (e.g. an
// anonymous/gated search response), so callers must not build a hashless magnet.
func magnetOf(t tc.TorrentInfo) (string, bool) {
	if t.MagnetURL != nil && *t.MagnetURL != "" {
		return *t.MagnetURL, true
	}
	if len(t.InfoHash) == 40 {
		return "magnet:?xt=urn:btih:" + t.InfoHash, true
	}
	return "", false
}

// firstStreamable scans results in order and returns the best (highest-scoring)
// playable release of the first result that has one — skipping results with no
// torrents or with only gated releases (no magnet/hash, e.g. an unauthenticated
// response). Used by the non-interactive `--stream` path so a hashless slot-0
// title doesn't abort a stream the next result could satisfy.
func firstStreamable(results []tc.SearchResult) (tc.TorrentInfo, bool) {
	for _, r := range results {
		if len(r.Torrents) == 0 {
			continue
		}
		t := bestTorrent(r)
		if _, ok := magnetOf(t); ok {
			return t, true
		}
	}
	return tc.TorrentInfo{}, false
}

// bestTorrent picks the highest-scoring release of a result, falling back to
// seeders on a tie. Assumes a non-empty Torrents slice (callers guard).
func bestTorrent(r tc.SearchResult) tc.TorrentInfo {
	best := r.Torrents[0]
	for _, t := range r.Torrents[1:] {
		if torrentRank(t) > torrentRank(best) {
			best = t
		}
	}
	return best
}

func torrentRank(t tc.TorrentInfo) int {
	score := 0
	if t.QualityScore != nil {
		score = *t.QualityScore
	}
	// Weight score above seeders but let seeders break ties within the same score.
	return score*1_000_000 + t.Seeders
}

func resultLabel(r tc.SearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)", r.Title, ui.FormatYear(r.Year))
	if r.RatingIMDb != nil && *r.RatingIMDb != "" {
		fmt.Fprintf(&b, " ⭐ %s", *r.RatingIMDb)
	}
	n := len(r.Torrents)
	if n == 1 {
		b.WriteString(" · 1 release")
	} else {
		fmt.Fprintf(&b, " · %d releases", n)
	}
	return b.String()
}

func torrentLabel(t tc.TorrentInfo) string {
	parts := []string{
		ui.StringOrDash(t.Quality),
		ui.FormatSize(t.SizeBytes),
		fmt.Sprintf("%d seeds", t.Seeders),
		t.Source,
	}
	if t.Codec != nil && *t.Codec != "" {
		parts = append(parts, *t.Codec)
	}
	if langs := ui.FormatLanguages(t.Languages); langs != "" && langs != "-" {
		parts = append(parts, langs)
	}
	if t.QualityScore != nil {
		parts = append(parts, fmt.Sprintf("score %d", *t.QualityScore))
	}
	return strings.Join(parts, " · ")
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
