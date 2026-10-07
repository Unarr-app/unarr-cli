// Package naming renders the library folder/file layout organize files a
// finished download into, from a small FileBot-inspired template language:
//
//		{n}< ({y})>< {imdb-{imdbid}}>/Season {s00}/{n} - {s00e00}< - {t}>
//
//	  - {token} is replaced by a value (see Tokens). Only a known token name in
//	    braces is a token: any other brace or bracket is literal text, so
//	    "{imdb-{imdbid}}" renders the Plex/Infuse tag "{imdb-tt0388629}" and
//	    "[imdbid-{imdbid}]" the Jellyfin one.
//	  - <…> is an optional segment, dropped whole when any token inside it has no
//	    value — "< ({y})>" vanishes for a title without a year instead of leaving
//	    "()" behind. '<' and '>' are invalid in Windows filenames, so they never
//	    need to be literal.
//	  - '/' separates folders; the last component is the file name, without
//	    extension (organize appends the real one).
//	  - {s} / {e} accept a zero-pad width: {e.pad(4)} → "0001".
//
// The package is pure (no I/O) so config validation, organize and the preview
// command all share one implementation.
package naming

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Vars holds the values a template is rendered with. Empty strings and nil
// numbers mean "unknown".
type Vars struct {
	Title        string
	Year         string
	ImdbID       string
	TmdbID       string
	TvdbID       string
	Collection   string
	EpisodeTitle string
	VideoFormat  string // "1080p", "2160p"…
	Season       *int
	Episode      *int
}

// Tokens documents every token name a template may use.
var Tokens = map[string]string{
	"n":          "title",
	"y":          "year",
	"imdbid":     "IMDb id (tt0388629)",
	"tmdbid":     "TMDB id",
	"tvdbid":     "TheTVDB id (shows)",
	"collection": "collection name (movies)",
	"t":          "episode title",
	"vf":         "video format (1080p)",
	"s":          "season number",
	"e":          "episode number",
	"s00":        "season, 2 digits",
	"e00":        "episode, at least 2 digits",
	"s00e00":     "S01E05",
	"sxe":        "1x05",
}

// episodeTokens are the tokens that identify an episode in a file name — the
// library scanner needs one of them to tell episodes apart.
var episodeTokens = map[string]bool{"e": true, "e00": true, "s00e00": true, "sxe": true}

type part struct {
	literal  string
	token    string // "" = literal part
	pad      int    // {s.pad(N)} / {e.pad(N)} width, 0 = none
	optional *[]part
}

// Template is a parsed naming template.
type Template struct {
	src   string
	parts []part
}

// String returns the template source.
func (t *Template) String() string { return t.src }

// tokenRe matches anything shaped like a token — `{word}` or `{word.modifier}`
// — so a typo ({N}, {Title}, {e.pad(10)}) is an error instead of silently
// rendering as literal text. "{imdb-…" is not token-shaped ('-'), so it stays
// a literal.
var (
	tokenRe = regexp.MustCompile(`^\{([A-Za-z0-9_]+)(\.[^{}]*)?\}`)
	padRe   = regexp.MustCompile(`^\.pad\(([1-9])\)$`)
)

// windowsInvalid are characters a literal may not contain: they are invalid in
// Windows/SMB filenames ('<' '>' are the optional-segment syntax).
const windowsInvalid = `\:*?"|`

// Parse parses and validates a template. series requires the file name to carry
// an episode token ({s00e00}, {e}, {e00} or {sxe}).
func Parse(src string, series bool) (*Template, error) {
	s := strings.TrimSpace(src)
	if err := checkSource(s); err != nil {
		return nil, err
	}
	p := &parser{src: s}
	p.cur = &p.top
	for p.pos < len(s) {
		if err := p.step(); err != nil {
			return nil, err
		}
	}
	if p.opt != nil {
		return nil, fmt.Errorf("'<' without closing '>'")
	}
	t := &Template{src: s, parts: mergeLiterals(p.top)}
	return t, t.validate(series)
}

// checkSource rejects templates that can never yield a safe relative path.
func checkSource(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("empty template")
	case strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/"):
		return fmt.Errorf("template must not start or end with '/'")
	case strings.ContainsAny(s, windowsInvalid):
		return fmt.Errorf(`template contains a character invalid in file names (one of %s)`, windowsInvalid)
	}
	return nil
}

// parser walks a template once, appending parts to the top level or to the
// optional segment being read.
type parser struct {
	src string
	pos int
	top []part
	opt *[]part // open <…> segment, nil outside one
	cur *[]part // where parts go: &top or opt
}

func (p *parser) step() error {
	switch c := p.src[p.pos]; c {
	case '<':
		if p.opt != nil {
			return fmt.Errorf("nested '<' at position %d", p.pos)
		}
		p.opt = &[]part{}
		p.cur = p.opt
	case '>':
		if p.opt == nil {
			return fmt.Errorf("'>' without '<' at position %d", p.pos)
		}
		p.top = append(p.top, part{optional: p.opt})
		p.opt, p.cur = nil, &p.top
	case '{':
		if m := tokenRe.FindStringSubmatch(p.src[p.pos:]); m != nil {
			tok, err := tokenPart(m[1], m[2])
			if err != nil {
				return err
			}
			*p.cur = append(*p.cur, tok)
			p.pos += len(m[0])
			return nil
		}
		*p.cur = append(*p.cur, part{literal: "{"})
	default:
		*p.cur = append(*p.cur, part{literal: string(c)})
	}
	p.pos++
	return nil
}

// tokenPart builds a token part from a `{name}` / `{name.pad(N)}` match.
func tokenPart(name, modifier string) (part, error) {
	if _, ok := Tokens[name]; !ok {
		return part{}, fmt.Errorf("unknown token {%s}%s", name, hint(name))
	}
	p := part{token: name}
	if modifier == "" {
		return p, nil
	}
	if name != "s" && name != "e" {
		return part{}, fmt.Errorf("{%s%s}: modifiers only apply to {s} and {e}", name, modifier)
	}
	m := padRe.FindStringSubmatch(modifier)
	if m == nil {
		return part{}, fmt.Errorf("{%s%s}: the only modifier is .pad(N), N from 1 to 9", name, modifier)
	}
	p.pad, _ = strconv.Atoi(m[1])
	return p, nil
}

func hint(name string) string {
	if lower := strings.ToLower(name); lower != name {
		if _, ok := Tokens[lower]; ok {
			return fmt.Sprintf(" (tokens are lowercase: {%s})", lower)
		}
		name = lower
	}
	switch name {
	case "imdb", "tmdb", "tvdb":
		return fmt.Sprintf(" (did you mean {%sid}?)", name)
	case "title", "name":
		return " (use {n})"
	case "year":
		return " (use {y})"
	}
	return ""
}

// validate checks the layout with every token filled, so literal structure
// ("..", empty folders) is caught at config time, not at the first download.
func (t *Template) validate(series bool) error {
	full, _ := renderAll(t.parts, sampleVars())
	full = strings.ReplaceAll(full, markPresent, "")
	for _, c := range strings.Split(full, "/") {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("template has an empty folder name (\"//\")")
		}
		if c == "." || c == ".." {
			return fmt.Errorf("template must not contain %q", c)
		}
	}
	all, required := lastComponentTokens(t.parts)
	if len(all) == 0 {
		return fmt.Errorf("file name (after the last '/') must contain at least one token")
	}
	if series {
		// Outside <…>: an optional episode number would let an unnumbered file
		// be renamed to the bare show name, and every such file collide.
		for _, tok := range required {
			if episodeTokens[tok] {
				return nil
			}
		}
		return fmt.Errorf("series file name must contain {s00e00}, {sxe}, {e} or {e00} outside <...> so episodes stay distinguishable")
	}
	return nil
}

// lastComponentTokens lists the tokens of the file name — after the last '/',
// wherever it is — and, separately, those not inside an optional segment.
func lastComponentTokens(parts []part) (all, required []string) {
	var walk func(ps []part, optional bool)
	walk = func(ps []part, optional bool) {
		for _, p := range ps {
			switch {
			case p.optional != nil:
				walk(*p.optional, true)
			case p.token != "":
				all = append(all, p.token)
				if !optional {
					required = append(required, p.token)
				}
			case strings.Contains(p.literal, "/"):
				all, required = all[:0], required[:0]
			}
		}
	}
	walk(parts, false)
	return all, required
}

func mergeLiterals(ps []part) []part {
	var out []part
	for _, p := range ps {
		if p.optional != nil {
			merged := mergeLiterals(*p.optional)
			p.optional = &merged
		}
		if n := len(out); n > 0 && p.token == "" && p.optional == nil && out[n-1].token == "" && out[n-1].optional == nil {
			out[n-1].literal += p.literal
			continue
		}
		out = append(out, p)
	}
	return out
}

// Result is a rendered layout: folders relative to the library root, and the
// file stem (no extension). FileOK is false when the file name needs a value
// the task doesn't have (e.g. an episode number) — the caller then keeps the
// download's original file name.
type Result struct {
	Dirs   []string
	File   string
	FileOK bool
}

// Render renders the template with v.
//
// A token without a value inside <…> drops that segment. Outside one it renders
// empty, and then: a folder left with no text at all is skipped (a "Season
// {s00}" folder for a show without seasons), empty "()"/"[]" are tidied away,
// and a file name with a missing value is reported as !FileOK.
func (t *Template) Render(v Vars) Result {
	out, _ := renderAll(t.parts, v)
	comps := strings.Split(out, "/")
	var res Result
	for i, c := range comps {
		missing := strings.Contains(c, markMissing)
		present := strings.Contains(c, markPresent)
		c = tidy(markStripper.Replace(c), missing)
		if i == len(comps)-1 {
			res.File = c // FitFileName bounds it once the extension is known
			res.FileOK = c != "" && !missing
			break
		}
		c = fitName(c, maxComponentBytes)
		// A folder made only of missing values ("Season {s00}" for a show with
		// no season) is skipped rather than created as a bare "Season".
		if c == "" || c == "." || c == ".." || (missing && !present) {
			continue
		}
		res.Dirs = append(res.Dirs, c)
	}
	return res
}

// Render marks where a token rendered a value / had none, so a component can
// be judged after the '/' split. renderAll strips both from values first.
const (
	markMissing = "\x00"
	markPresent = "\x01"
)

var markStripper = strings.NewReplacer(markMissing, "", markPresent, "")

// renderAll renders parts; ok is false when a token had no value. Outside an
// optional segment that leaves a markMissing in the output.
func renderAll(ps []part, v Vars) (string, bool) {
	var b strings.Builder
	ok := true
	for _, p := range ps {
		switch {
		case p.optional != nil:
			if seg, segOK := renderAll(*p.optional, v); segOK {
				b.WriteString(seg)
			}
		case p.token != "":
			val := markStripper.Replace(value(p, v))
			if val == "" {
				ok = false
				b.WriteString(markMissing)
				continue
			}
			b.WriteString(markPresent)
			b.WriteString(val)
		default:
			b.WriteString(p.literal)
		}
	}
	return b.String(), ok
}

func value(p part, v Vars) string {
	if f, ok := tokenValues[p.token]; ok {
		return f(v, p.pad)
	}
	return ""
}

// tokenValues renders each token in Tokens; pad is the {s.pad(N)} width.
var tokenValues = map[string]func(v Vars, pad int) string{
	"n":          func(v Vars, _ int) string { return Sanitize(v.Title) },
	"y":          func(v Vars, _ int) string { return Sanitize(v.Year) },
	"imdbid":     func(v Vars, _ int) string { return Sanitize(v.ImdbID) },
	"tmdbid":     func(v Vars, _ int) string { return Sanitize(v.TmdbID) },
	"tvdbid":     func(v Vars, _ int) string { return Sanitize(v.TvdbID) },
	"collection": func(v Vars, _ int) string { return Sanitize(v.Collection) },
	"t":          func(v Vars, _ int) string { return Sanitize(v.EpisodeTitle) },
	"vf":         func(v Vars, _ int) string { return Sanitize(v.VideoFormat) },
	"s":          func(v Vars, pad int) string { return padded(v.Season, max(pad, 1)) },
	"e":          func(v Vars, pad int) string { return padded(v.Episode, max(pad, 1)) },
	"s00":        func(v Vars, _ int) string { return padded(v.Season, 2) },
	"e00":        func(v Vars, _ int) string { return padded(v.Episode, 2) },
	"s00e00": func(v Vars, _ int) string {
		return episodeCode(v, func(s, e int) string { return fmt.Sprintf("S%02dE%02d", s, e) })
	},
	"sxe": func(v Vars, _ int) string {
		return episodeCode(v, func(s, e int) string { return fmt.Sprintf("%dx%02d", s, e) })
	},
}

func padded(n *int, width int) string {
	if n == nil || *n < 0 {
		return ""
	}
	return fmt.Sprintf("%0*d", width, *n)
}

func episodeCode(v Vars, format func(s, e int) string) string {
	if v.Season == nil || v.Episode == nil || *v.Season < 0 || *v.Episode < 0 {
		return ""
	}
	return format(*v.Season, *v.Episode)
}

var (
	emptyBrackets = regexp.MustCompile(`\s*(\(\s*\)|\[\s*\]|\{\s*\})`)
	multiSpace    = regexp.MustCompile(`\s{2,}`)
)

// maxComponentBytes is the common per-name limit (ext4, NTFS, APFS: 255). A
// longer name could never be created, so cutting it changes nothing that used
// to work.
const maxComponentBytes = 255

// tidy cleans what a missing value left behind ("Show ()", "Show  - S01E01",
// "- S01E01"). A component whose every value was present is returned as
// rendered: the default layout must keep producing byte-identical names, and
// a real title may well hold double spaces, a leading '-' or a trailing dot.
func tidy(c string, missing bool) string {
	if !missing {
		return c
	}
	c = emptyBrackets.ReplaceAllString(c, "")
	c = multiSpace.ReplaceAllString(c, " ")
	c = strings.TrimSpace(c)
	c = strings.TrimRight(c, ". ")
	return strings.TrimLeft(c, " -")
}

// fitName cuts name to at most limit bytes on a rune boundary. Only a name the
// filesystem would reject anyway is ever touched.
func fitName(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	cut := max(limit, 0)
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return strings.TrimRight(name[:cut], ". ")
}

// FitFileName joins a rendered file stem and its extension, cutting the stem's
// END when the whole name would exceed the per-name limit: a too-long episode
// title is shortened, the "Show - S01E01" ahead of it survives.
func FitFileName(stem, ext string) string {
	return fitName(stem, maxComponentBytes-len(ext)) + ext
}

var valueReplacer = strings.NewReplacer(
	"/", "-",
	"\\", "-",
	":", " -",
	"?", "",
	"*", "",
	"\"", "",
	"<", "",
	">", "",
	"|", "-",
)

// Sanitize makes a value safe as part of a file or folder name: path
// separators and characters invalid on Windows/SMB are replaced or dropped,
// surrounding spaces and trailing dots trimmed.
func Sanitize(s string) string {
	s = valueReplacer.Replace(s)
	s = strings.TrimSpace(s)
	return strings.TrimRight(s, ".")
}

func sampleVars() Vars {
	one := 1
	return Vars{
		Title: "Title", Year: "2000", ImdbID: "tt0000000", TmdbID: "1", TvdbID: "1",
		Collection: "Collection", EpisodeTitle: "Episode", VideoFormat: "1080p",
		Season: &one, Episode: &one,
	}
}
