package naming

import "path/filepath"

// Sample is one example download the preview renders a scheme against.
type Sample struct {
	Label  string
	Series bool
	Vars   Vars
}

// Samples are the preview fixtures: the One Piece homonyms that motivated
// templates, a movie with no ids, a collection film, a 4-digit absolute
// episode and an unnumbered episode.
func Samples() []Sample {
	ep := func(s, e int) (*int, *int) { return &s, &e }
	s1, e1 := ep(1, 1)
	s22, e1089 := ep(22, 1089)
	s1b, _ := ep(1, 0)
	return []Sample{
		{Label: "movie", Vars: Vars{Title: "One Piece: The Movie", Year: "2000", ImdbID: "tt0814243", TmdbID: "19576", VideoFormat: "1080p"}},
		{Label: "movie, no ids", Vars: Vars{Title: "Some Film", Year: "2021", VideoFormat: "1080p"}},
		{Label: "collection movie", Vars: Vars{Title: "Knives Out", Year: "2019", ImdbID: "tt8946378", Collection: "Knives Out Collection", VideoFormat: "2160p"}},
		{Label: "anime episode", Series: true, Vars: Vars{Title: "One Piece", Year: "1999", ImdbID: "tt0388629", TmdbID: "37854", TvdbID: "81797",
			EpisodeTitle: "I'm Luffy! The Man Who Will Become the Pirate King!", Season: s1, Episode: e1, VideoFormat: "1080p"}},
		{Label: "same-title show", Series: true, Vars: Vars{Title: "ONE PIECE", Year: "2023", ImdbID: "tt11737520", TmdbID: "111110", TvdbID: "392276",
			EpisodeTitle: "ROMANCE DAWN", Season: s1, Episode: e1, VideoFormat: "2160p"}},
		{Label: "4-digit episode", Series: true, Vars: Vars{Title: "One Piece", Year: "1999", ImdbID: "tt0388629",
			EpisodeTitle: "Entering a New Chapter! Luffy and Sabo's Paths!", Season: s22, Episode: e1089, VideoFormat: "1080p"}},
		{Label: "unnumbered episode", Series: true, Vars: Vars{Title: "One Piece", Year: "1999", ImdbID: "tt0388629", Season: s1b}},
	}
}

// OriginalName stands in for the download's own file name in a preview, when
// the template can't name the file.
const OriginalName = "<original file name>"

// PreviewLine is one rendered sample.
type PreviewLine struct {
	Label string
	Path  string
}

// Preview renders every sample under movieRoot / seriesRoot with extension ext.
func (s Scheme) Preview(movieRoot, seriesRoot, ext string) []PreviewLine {
	var out []PreviewLine
	for _, smp := range Samples() {
		tpl, root := s.Movie, movieRoot
		if smp.Series {
			tpl, root = s.Series, seriesRoot
		}
		r := tpl.Render(smp.Vars)
		file := OriginalName
		if r.FileOK {
			file = FitFileName(r.File, ext)
		}
		parts := append([]string{root}, r.Dirs...)
		out = append(out, PreviewLine{Label: smp.Label, Path: filepath.Join(append(parts, file)...)})
	}
	return out
}
