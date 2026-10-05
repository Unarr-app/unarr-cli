package naming

import (
	"fmt"
	"sort"
	"strings"
)

// Preset is a named pair of movie/series templates.
type Preset struct {
	Description string
	Movie       string
	Series      string
}

// DefaultPreset is the layout organize always used; it must keep rendering the
// exact same paths so existing libraries never split (see naming_test golden).
const DefaultPreset = "default"

// Presets are the built-in layouts. Media servers match far better with an id
// in the folder name: same-titled shows (One Piece 1999 anime vs the 2023
// live action) otherwise collide or get the wrong metadata.
var Presets = map[string]Preset{
	DefaultPreset: {
		Description: "Title (Year)/Title (Year), Show/Season 01/Show - S01E01",
		Movie:       "<{collection}/>{n}< ({y})>/{n}< ({y})>",
		Series:      "{n}/Season {s00}/{n} - {s00e00}",
	},
	"plex": {
		Description: "Plex, Infuse, Emby: ids as {imdb-tt...}, year on shows, episode titles",
		Movie:       "<{collection}/>{n}< ({y})>< {imdb-{imdbid}}>/{n}< ({y})>< {imdb-{imdbid}}>",
		Series:      "{n}< ({y})>< {imdb-{imdbid}}>/Season {s00}/{n}< ({y})> - {s00e00}< - {t}>",
	},
	"jellyfin": {
		Description: "Jellyfin: ids as [imdbid-tt...], year on shows, episode titles",
		Movie:       "<{collection}/>{n}< ({y})>< [imdbid-{imdbid}]>/{n}< ({y})>< [imdbid-{imdbid}]>",
		Series:      "{n}< ({y})>< [imdbid-{imdbid}]>/Season {s00}/{n}< ({y})> - {s00e00}< - {t}>",
	},
}

// presetAliases map other player names onto the preset that suits them.
var presetAliases = map[string]string{"infuse": "plex", "emby": "plex"}

// PresetNames lists the accepted preset names (aliases included), sorted.
func PresetNames() []string {
	names := make([]string, 0, len(Presets)+len(presetAliases))
	for n := range Presets {
		names = append(names, n)
	}
	for n := range presetAliases {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Scheme is the pair of parsed templates organize renders with.
type Scheme struct {
	Movie  *Template
	Series *Template
}

// Resolve builds the scheme for a preset name ("" = default), with the
// non-empty movie/series formats overriding the preset's template.
func Resolve(preset, movieFormat, seriesFormat string) (Scheme, error) {
	name := strings.ToLower(strings.TrimSpace(preset))
	if name == "" {
		name = DefaultPreset
	}
	if alias, ok := presetAliases[name]; ok {
		name = alias
	}
	p, ok := Presets[name]
	if !ok {
		return Scheme{}, fmt.Errorf("unknown naming preset %q (use one of: %s)", preset, strings.Join(PresetNames(), ", "))
	}
	movieSrc, seriesSrc := p.Movie, p.Series
	if strings.TrimSpace(movieFormat) != "" {
		movieSrc = movieFormat
	}
	if strings.TrimSpace(seriesFormat) != "" {
		seriesSrc = seriesFormat
	}
	movie, err := Parse(movieSrc, false)
	if err != nil {
		return Scheme{}, fmt.Errorf("movie_format: %w", err)
	}
	series, err := Parse(seriesSrc, true)
	if err != nil {
		return Scheme{}, fmt.Errorf("series_format: %w", err)
	}
	return Scheme{Movie: movie, Series: series}, nil
}

// Default returns the default scheme. The built-in templates always parse.
func Default() Scheme {
	s, err := Resolve(DefaultPreset, "", "")
	if err != nil {
		panic(err)
	}
	return s
}
