package arr

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// TestDiscoverE2E is an integration test that requires real *arr instances running.
// Skip if ports 8989/7878 are not reachable.
func TestDiscoverE2E(t *testing.T) {
	if os.Getenv("ARR_E2E") == "" {
		t.Skip("Set ARR_E2E=1 to run integration tests")
	}

	// Check ports are reachable
	for _, port := range []string{"8989", "7878"} {
		conn, err := net.DialTimeout("tcp", "localhost:"+port, 2*time.Second)
		if err != nil {
			t.Skipf("Port %s not reachable, skipping", port)
		}
		_ = conn.Close()
	}

	t.Run("Discover", func(t *testing.T) {
		instances := Discover()
		if len(instances) == 0 {
			t.Fatal("Discover() returned 0 instances")
		}
		for _, inst := range instances {
			t.Logf("Found: %s at %s (source=%s, version=%s, hasKey=%v)",
				inst.App, inst.URL, inst.Source, inst.Version, inst.APIKey != "")
		}
	})

	t.Run("Radarr_Movies", func(t *testing.T) {
		radarrKey := os.Getenv("RADARR_KEY")
		if radarrKey == "" {
			t.Skip("RADARR_KEY not set")
		}
		client := NewClient("http://localhost:7878", radarrKey)

		status, err := client.SystemStatus()
		if err != nil {
			t.Fatalf("SystemStatus: %v", err)
		}
		t.Logf("Radarr %s", status.Version)

		movies, err := client.Movies()
		if err != nil {
			t.Fatalf("Movies: %v", err)
		}
		t.Logf("Found %d movies", len(movies))
		for _, m := range movies {
			t.Logf("  %s (tmdb=%d, imdb=%s) monitored=%v hasFile=%v profile=%d",
				m.Title, m.TmdbID, m.ImdbID, m.Monitored, m.HasFile, m.QualityProfileID)
		}
		if len(movies) < 3 {
			t.Errorf("Expected at least 3 movies, got %d", len(movies))
		}

		profiles, err := client.QualityProfiles()
		if err != nil {
			t.Fatalf("QualityProfiles: %v", err)
		}
		t.Logf("Found %d quality profiles", len(profiles))
		for _, p := range profiles {
			mapped := MapQualityProfile(p)
			t.Logf("  %s (id=%d) → %s", p.Name, p.ID, mapped)
		}

		folders, err := client.RootFolders()
		if err != nil {
			t.Fatalf("RootFolders: %v", err)
		}
		t.Logf("Found %d root folders", len(folders))
		for _, f := range folders {
			t.Logf("  %s", f.Path)
		}

		dcs, err := client.DownloadClients()
		if err != nil {
			t.Fatalf("DownloadClients: %v", err)
		}
		t.Logf("Found %d download clients", len(dcs))

		// Test wanted extraction
		wanted := ExtractWantedMovies(movies)
		t.Logf("Wanted movies: %d", len(wanted))
		for _, w := range wanted {
			t.Logf("  %s (tmdb=%d)", w.Title, w.TmdbID)
		}
		if len(wanted) != 2 {
			t.Errorf("Expected 2 wanted movies, got %d", len(wanted))
		}
	})

	t.Run("Sonarr_Series", func(t *testing.T) {
		sonarrKey := os.Getenv("SONARR_KEY")
		if sonarrKey == "" {
			t.Skip("SONARR_KEY not set")
		}
		client := NewClient("http://localhost:8989", sonarrKey)

		status, err := client.SystemStatus()
		if err != nil {
			t.Fatalf("SystemStatus: %v", err)
		}
		t.Logf("Sonarr %s", status.Version)

		series, err := client.Series()
		if err != nil {
			t.Fatalf("Series: %v", err)
		}
		t.Logf("Found %d series", len(series))
		for _, s := range series {
			t.Logf("  %s (tvdb=%d, imdb=%s) monitored=%v eps=%d/%d",
				s.Title, s.TvdbID, s.ImdbID, s.Monitored,
				s.Statistics.EpisodeFileCount, s.Statistics.EpisodeCount)
		}

		wanted := ExtractWantedSeries(series)
		t.Logf("Wanted series: %d", len(wanted))
		for _, w := range wanted {
			t.Logf("  %s (imdb=%s)", w.Title, w.ImdbID)
		}
		if len(wanted) != 2 {
			t.Errorf("Expected 2 wanted series, got %d", len(wanted))
		}
	})

	t.Run("BuildMigrationResult", func(t *testing.T) {
		radarrKey := os.Getenv("RADARR_KEY")
		sonarrKey := os.Getenv("SONARR_KEY")
		if radarrKey == "" || sonarrKey == "" {
			t.Skip("RADARR_KEY and SONARR_KEY required")
		}

		rc := NewClient("http://localhost:7878", radarrKey)
		sc := NewClient("http://localhost:8989", sonarrKey)

		movies, _ := rc.Movies()
		series, _ := sc.Series()
		rp, _ := rc.QualityProfiles()
		sp, _ := sc.QualityProfiles()
		rf, _ := rc.RootFolders()
		sf, _ := sc.RootFolders()
		dcs, _ := rc.DownloadClients()

		result := BuildMigrationResult(movies, series, rp, sp, rf, sf, nil, dcs)

		fmt.Printf("\n=== Migration Result ===\n")
		fmt.Printf("  MoviesDir:     %s\n", result.MoviesDir)
		fmt.Printf("  TVShowsDir:    %s\n", result.TVShowsDir)
		fmt.Printf("  Quality:       %s (from %q)\n", result.Quality, result.QualitySource)
		fmt.Printf("  Organize:      %v\n", result.OrganizeEnabled)
		fmt.Printf("  Movies:        %d total, %d with files\n", result.TotalMovies, result.MoviesWithFiles)
		fmt.Printf("  Series:        %d total, %d complete\n", result.TotalSeries, result.SeriesComplete)
		fmt.Printf("  Wanted movies: %d\n", len(result.WantedMovies))
		fmt.Printf("  Wanted series: %d\n", len(result.WantedSeries))

		if result.MoviesDir != "/data/media/movies" {
			t.Errorf("MoviesDir = %q, want /data/media/movies", result.MoviesDir)
		}
		if result.TVShowsDir != "/data/media/tv" {
			t.Errorf("TVShowsDir = %q, want /data/media/tv", result.TVShowsDir)
		}
		if len(result.WantedMovies) != 2 {
			t.Errorf("WantedMovies = %d, want 2", len(result.WantedMovies))
		}
		if len(result.WantedSeries) != 2 {
			t.Errorf("WantedSeries = %d, want 2", len(result.WantedSeries))
		}
		if !result.OrganizeEnabled {
			t.Error("OrganizeEnabled should be true")
		}
	})
}
