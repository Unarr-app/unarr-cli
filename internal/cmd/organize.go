package cmd

import (
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/naming"
)

// organizeNaming is the naming scheme organize files new downloads with. An
// invalid preset/template must not stop the daemon: it falls back to the
// default layout, loudly (`unarr config check` reports the same problem).
func organizeNaming(cfg config.Config) naming.Scheme {
	scheme, err := cfg.Organize.NamingScheme()
	if err != nil {
		log.Printf("[organize] invalid naming config, using the default layout: %v", err)
		return naming.Default()
	}
	return scheme
}

func newOrganizeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "organize",
		GroupID: "system",
		Short:   "How downloads are named and filed into your library",
	}
	cmd.AddCommand(newOrganizePreviewCmd())
	return cmd
}

func newOrganizePreviewCmd() *cobra.Command {
	var preset, movieFormat, seriesFormat string
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Show the folders and file names new downloads would get",
		Long: `Renders the naming layout against sample downloads (same-titled shows,
a movie without ids, a collection, a 4-digit episode) without touching any file.

With no flags it previews your configured [organize] naming; the flags try
another preset or template first. Presets: ` + strings.Join(naming.PresetNames(), ", ") + `.

Template tokens: {n} title, {y} year, {imdbid}, {tmdbid}, {tvdbid},
{collection}, {s} {e} {s00} {e00} {s00e00} {sxe}, {t} episode title,
{vf} 1080p, {e.pad(4)}. Wrap a part in <...> to drop it when a value is missing:
  unarr organize preview --series-format "{n}< ({y})>< {tvdb-{tvdbid}}>/Season {s00}/{n} - {s00e00}< - {t}>"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o := loadConfig().Organize
			if cmd.Flags().Changed("naming") {
				o.Naming, o.MovieFormat, o.SeriesFormat = preset, "", ""
			}
			if movieFormat != "" {
				o.MovieFormat = movieFormat
			}
			if seriesFormat != "" {
				o.SeriesFormat = seriesFormat
			}
			scheme, err := o.NamingScheme()
			if err != nil {
				return err
			}
			printNamingPreview(cmd.OutOrStdout(), o, scheme)
			return nil
		},
	}
	cmd.Flags().StringVar(&preset, "naming", "", "preview a preset instead of the configured one ("+strings.Join(naming.PresetNames(), ", ")+")")
	cmd.Flags().StringVar(&movieFormat, "movie-format", "", "preview a custom movie template")
	cmd.Flags().StringVar(&seriesFormat, "series-format", "", "preview a custom series template")
	return cmd
}

// namingFormGroup is the naming part of `unarr config` → organization: the
// preset, the custom templates (validated as typed) and a live preview of the
// resulting paths under the current choices.
func namingFormGroup(o *config.OrganizeConfig) *huh.Group {
	if o.Naming == "" {
		o.Naming = naming.DefaultPreset
	}
	presets := make([]huh.Option[string], 0, len(naming.Presets))
	for _, name := range []string{naming.DefaultPreset, "plex", "jellyfin"} {
		presets = append(presets, huh.NewOption(name+" - "+naming.Presets[name].Description, name))
	}
	validate := func(series bool) func(string) error {
		return func(s string) error {
			if strings.TrimSpace(s) == "" {
				return nil
			}
			_, err := naming.Parse(s, series)
			return err
		}
	}
	return huh.NewGroup(
		huh.NewSelect[string]().
			Title("File naming").
			Description("How new downloads are named. Files already in your library are never moved.").
			Options(presets...).
			Value(&o.Naming),
		huh.NewInput().
			Title("Custom movie template (blank = preset)").
			Placeholder(naming.Presets["plex"].Movie).
			Validate(validate(false)).
			Value(&o.MovieFormat),
		huh.NewInput().
			Title("Custom series template (blank = preset)").
			Placeholder(naming.Presets["plex"].Series).
			Validate(validate(true)).
			Value(&o.SeriesFormat),
		huh.NewNote().
			Title("Preview").
			DescriptionFunc(func() string { return namingPreviewText(*o) }, o),
	)
}

func namingPreviewText(o config.OrganizeConfig) string {
	scheme, err := o.NamingScheme()
	if err != nil {
		return "! " + err.Error()
	}
	var b strings.Builder
	for _, l := range scheme.Preview("Movies", "TV Shows", ".mkv") {
		fmt.Fprintf(&b, "%s\n", l.Path)
	}
	b.WriteString("Tokens: {n} {y} {imdbid} {tmdbid} {tvdbid} {collection} {s00e00} {t} {vf} - <...> = optional")
	return b.String()
}

func printNamingPreview(w io.Writer, o config.OrganizeConfig, scheme naming.Scheme) {
	movies, shows := o.MoviesDir, o.TVShowsDir
	if movies == "" {
		movies = "Movies"
	}
	if shows == "" {
		shows = "TV Shows"
	}
	bold := color.New(color.Bold).SprintFunc()
	dim := color.New(color.Faint).SprintFunc()
	fmt.Fprintf(w, "%s %s\n", bold("Movies:"), scheme.Movie)
	fmt.Fprintf(w, "%s %s\n\n", bold("Series:"), scheme.Series)
	for _, l := range scheme.Preview(movies, shows, ".mkv") {
		fmt.Fprintf(w, "  %-20s %s\n", dim(l.Label), l.Path)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, dim("Only new downloads use this layout - files already in your library are never moved."))
	if scheme.Series.String() != naming.Default().Series.String() {
		fmt.Fprintln(w, dim("Shows you already have keep their current folder; new episodes of them go to the folder above."))
	}
	if !o.Enabled {
		fmt.Fprintln(w, color.YellowString("Note: [organize] enabled = false - downloads are not being organized."))
	}
	fmt.Fprintln(w, dim("Set it under [organize] (naming / movie_format / series_format) or with `unarr config`, then restart the daemon."))
}
