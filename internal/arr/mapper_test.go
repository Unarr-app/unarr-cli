package arr

import (
	"testing"
)

func TestMapQualityProfile_2160p(t *testing.T) {
	profile := QualityProfile{
		ID:     1,
		Name:   "Ultra-HD",
		Cutoff: 31, // 2160p Remux
		Items: []QualityItem{
			{Quality: &Quality{ID: 31, Name: "Remux-2160p", Resolution: 2160}, Allowed: true},
			{Quality: &Quality{ID: 18, Name: "HDTV-2160p", Resolution: 2160}, Allowed: true},
			{Quality: &Quality{ID: 7, Name: "Bluray-1080p", Resolution: 1080}, Allowed: true},
		},
	}
	if got := MapQualityProfile(profile); got != "2160p" {
		t.Errorf("MapQualityProfile = %q, want 2160p", got)
	}
}

func TestMapQualityProfile_1080p(t *testing.T) {
	profile := QualityProfile{
		ID:     2,
		Name:   "HD-1080p",
		Cutoff: 7,
		Items: []QualityItem{
			{Quality: &Quality{ID: 7, Name: "Bluray-1080p", Resolution: 1080}, Allowed: true},
			{Quality: &Quality{ID: 3, Name: "HDTV-720p", Resolution: 720}, Allowed: true},
		},
	}
	if got := MapQualityProfile(profile); got != "1080p" {
		t.Errorf("MapQualityProfile = %q, want 1080p", got)
	}
}

func TestMapQualityProfile_720p_fallback(t *testing.T) {
	profile := QualityProfile{
		ID:     3,
		Name:   "SD",
		Cutoff: 1,
		Items: []QualityItem{
			{Quality: &Quality{ID: 1, Name: "SDTV", Resolution: 480}, Allowed: true},
		},
	}
	if got := MapQualityProfile(profile); got != "720p" {
		t.Errorf("MapQualityProfile = %q, want 720p", got)
	}
}

func TestMapQualityProfile_NestedGroups(t *testing.T) {
	profile := QualityProfile{
		ID:     4,
		Name:   "Any",
		Cutoff: 7,
		Items: []QualityItem{
			{
				Items: []QualityItem{
					{Quality: &Quality{ID: 7, Name: "Bluray-1080p", Resolution: 1080}, Allowed: true},
					{Quality: &Quality{ID: 3, Name: "HDTV-720p", Resolution: 720}, Allowed: true},
				},
			},
		},
	}
	if got := MapQualityProfile(profile); got != "1080p" {
		t.Errorf("MapQualityProfile = %q, want 1080p", got)
	}
}

func TestMostUsedProfile(t *testing.T) {
	profiles := []QualityProfile{
		{ID: 1, Name: "HD-1080p"},
		{ID: 2, Name: "Ultra-HD"},
		{ID: 3, Name: "SD"},
	}
	counts := map[int]int{1: 5, 2: 20, 3: 3}

	p := MostUsedProfile(counts, profiles)
	if p == nil || p.ID != 2 {
		t.Errorf("MostUsedProfile = %v, want profile ID 2", p)
	}
}

func TestMostUsedProfile_EmptyCounts(t *testing.T) {
	profiles := []QualityProfile{
		{ID: 1, Name: "HD-1080p"},
	}
	p := MostUsedProfile(map[int]int{}, profiles)
	if p == nil || p.ID != 1 {
		t.Errorf("MostUsedProfile with empty counts should return first profile")
	}
}

func TestExtractWantedMovies(t *testing.T) {
	movies := []Movie{
		{TmdbID: 1, Title: "Has file", Monitored: true, HasFile: true},
		{TmdbID: 2, Title: "Wanted", Monitored: true, HasFile: false},
		{TmdbID: 3, Title: "Unmonitored", Monitored: false, HasFile: false},
		{TmdbID: 0, Title: "No TMDB", Monitored: true, HasFile: false}, // no tmdbId
	}
	wanted := ExtractWantedMovies(movies)
	if len(wanted) != 1 {
		t.Fatalf("ExtractWantedMovies = %d items, want 1", len(wanted))
	}
	if wanted[0].TmdbID != 2 || wanted[0].Type != "movie" {
		t.Errorf("ExtractWantedMovies[0] = %+v, want tmdbId=2 type=movie", wanted[0])
	}
}

func TestExtractWantedSeries(t *testing.T) {
	series := []Series{
		{ImdbID: "tt1", Title: "Complete", Monitored: true, Statistics: SeriesStatistics{EpisodeCount: 10, EpisodeFileCount: 10}},
		{ImdbID: "tt2", Title: "Missing eps", Monitored: true, Statistics: SeriesStatistics{EpisodeCount: 10, EpisodeFileCount: 5}},
		{ImdbID: "tt3", Title: "Unmonitored", Monitored: false, Statistics: SeriesStatistics{EpisodeCount: 10, EpisodeFileCount: 0}},
	}
	wanted := ExtractWantedSeries(series)
	if len(wanted) != 1 {
		t.Fatalf("ExtractWantedSeries = %d items, want 1", len(wanted))
	}
	if wanted[0].ImdbID != "tt2" || wanted[0].Type != "show" {
		t.Errorf("ExtractWantedSeries[0] = %+v, want imdbId=tt2 type=show", wanted[0])
	}
}

func TestExtractBlocklistedHashes(t *testing.T) {
	items := []BlocklistItem{
		{Data: BlocklistData{InfoHash: "AAAA"}},
		{Data: BlocklistData{InfoHash: "AAAA"}}, // duplicate
		{Data: BlocklistData{InfoHash: "BBBB"}},
		{Data: BlocklistData{InfoHash: ""}}, // empty
	}
	hashes := ExtractBlocklistedHashes(items)
	if len(hashes) != 2 {
		t.Fatalf("ExtractBlocklistedHashes = %d, want 2", len(hashes))
	}
}

func TestExtractDownloadedHashes(t *testing.T) {
	records := []HistoryRecord{
		{EventType: "downloadFolderImported", Data: HistoryData{InfoHash: "hash1"}},
		{EventType: "grabbed", Data: HistoryData{InfoHash: "hash2"}},                // not imported
		{EventType: "downloadFolderImported", Data: HistoryData{InfoHash: "hash1"}}, // duplicate
		{EventType: "downloadFolderImported", Data: HistoryData{InfoHash: "hash3"}},
	}
	hashes := ExtractDownloadedHashes(records)
	if len(hashes) != 2 {
		t.Fatalf("ExtractDownloadedHashes = %d, want 2", len(hashes))
	}
}

func TestMapRootFolders_MostUsed(t *testing.T) {
	folders := []RootFolder{
		{Path: "/data/movies1"},
		{Path: "/data/movies2"},
	}
	movies := []Movie{
		{RootFolderPath: "/data/movies1"},
		{RootFolderPath: "/data/movies2"},
		{RootFolderPath: "/data/movies2"},
		{RootFolderPath: "/data/movies2"},
	}
	moviesDir, _ := MapRootFolders(folders, nil, movies, nil)
	if moviesDir != "/data/movies2" {
		t.Errorf("MapRootFolders = %q, want /data/movies2", moviesDir)
	}
}

func TestMapRootFolders_SingleFolder(t *testing.T) {
	folders := []RootFolder{{Path: "/data/movies"}}
	moviesDir, _ := MapRootFolders(folders, nil, nil, nil)
	if moviesDir != "/data/movies" {
		t.Errorf("MapRootFolders = %q, want /data/movies", moviesDir)
	}
}

func TestMapRootFolders_Empty(t *testing.T) {
	moviesDir, tvDir := MapRootFolders(nil, nil, nil, nil)
	if moviesDir != "" || tvDir != "" {
		t.Errorf("MapRootFolders empty = %q, %q, want empty", moviesDir, tvDir)
	}
}

func TestHasDockerPaths(t *testing.T) {
	tests := []struct {
		name     string
		movies   string
		tv       string
		expected bool
	}{
		{"docker paths", "/data/media/movies", "/data/media/tv", true},
		{"host paths", "/home/user/Media/Movies", "/home/user/Media/TV", false},
		{"empty", "", "", false},
		{"mixed", "/home/user/movies", "/data/media/tv", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &MigrationResult{MoviesDir: tt.movies, TVShowsDir: tt.tv}
			if got := HasDockerPaths(r); got != tt.expected {
				t.Errorf("HasDockerPaths = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestExtractDebridTokens(t *testing.T) {
	clients := []DownloadClient{
		{ID: 1, Name: "TorBox", Enable: true, Implementation: "TorBox", ImplementationName: "TorBox"},
		{ID: 2, Name: "qBittorrent", Enable: true, Implementation: "QBittorrent", ImplementationName: "qBittorrent"},
		{ID: 3, Name: "Disabled Debrid", Enable: false, Implementation: "RealDebrid", ImplementationName: "Real-Debrid"},
	}

	getFields := func(id int) []Field {
		if id == 1 {
			return []Field{
				{Name: "ApiKey", Value: "tb_test_token_123"},
				{Name: "Host", Value: "torbox.app"},
			}
		}
		return nil
	}

	tokens := ExtractDebridTokens(clients, getFields)
	if len(tokens) != 1 {
		t.Fatalf("ExtractDebridTokens = %d tokens, want 1", len(tokens))
	}
	if tokens[0].Provider != "torbox" || tokens[0].Token != "tb_test_token_123" {
		t.Errorf("ExtractDebridTokens[0] = %+v, want torbox with token", tokens[0])
	}
}
