package arr

import "strings"

// MapQualityProfile determines the preferred resolution from a quality profile.
// Uses the cutoff quality as the primary signal (what the user "wants"),
// falling back to the highest allowed resolution.
func MapQualityProfile(profile QualityProfile) string {
	maxResolution := 0
	cutoffResolution := 0

	var walk func(items []QualityItem)
	walk = func(items []QualityItem) {
		for _, item := range items {
			if len(item.Items) > 0 {
				walk(item.Items)
				continue
			}
			if item.Quality == nil || !item.Allowed {
				continue
			}
			if item.Quality.Resolution > maxResolution {
				maxResolution = item.Quality.Resolution
			}
			if item.Quality.ID == profile.Cutoff && item.Quality.Resolution > 0 {
				cutoffResolution = item.Quality.Resolution
			}
		}
	}
	walk(profile.Items)

	// Prefer the cutoff (what user wants), fall back to max allowed
	res := cutoffResolution
	if res == 0 {
		res = maxResolution
	}

	switch {
	case res >= 2160:
		return "2160p"
	case res >= 1080:
		return "1080p"
	default:
		return "720p"
	}
}

// MostUsedProfile finds the quality profile used by the most items.
func MostUsedProfile(profileCounts map[int]int, profiles []QualityProfile) *QualityProfile {
	if len(profiles) == 0 {
		return nil
	}

	bestID := profiles[0].ID
	bestCount := 0

	for id, count := range profileCounts {
		if count > bestCount {
			bestCount = count
			bestID = id
		}
	}

	for i := range profiles {
		if profiles[i].ID == bestID {
			return &profiles[i]
		}
	}
	return &profiles[0]
}

// MapRootFolders picks the most-used root folder from each app.
// Uses item paths to count which root folder is most popular.
func MapRootFolders(radarrFolders []RootFolder, sonarrFolders []RootFolder, movies []Movie, series []Series) (moviesDir, tvDir string) {
	moviesDir = mostUsedFolder(radarrFolders, func() []string {
		paths := make([]string, len(movies))
		for i, m := range movies {
			paths[i] = m.RootFolderPath
		}
		return paths
	}())
	tvDir = mostUsedFolder(sonarrFolders, func() []string {
		paths := make([]string, len(series))
		for i, s := range series {
			paths[i] = s.RootFolderPath
		}
		return paths
	}())
	return moviesDir, tvDir
}

// mostUsedFolder returns the folder path used by the most items.
// Falls back to the first folder if no items reference any folder.
func mostUsedFolder(folders []RootFolder, itemPaths []string) string {
	if len(folders) == 0 {
		return ""
	}
	if len(folders) == 1 {
		return folders[0].Path
	}

	counts := map[string]int{}
	for _, p := range itemPaths {
		if p != "" {
			counts[p]++
		}
	}

	best := folders[0].Path
	bestCount := 0
	for _, f := range folders {
		if c := counts[f.Path]; c > bestCount {
			bestCount = c
			best = f.Path
		}
	}
	return best
}

// ExtractWantedMovies returns movies that are monitored but missing files.
func ExtractWantedMovies(movies []Movie) []WantedItem {
	var wanted []WantedItem
	for _, m := range movies {
		if m.Monitored && !m.HasFile && m.TmdbID > 0 {
			wanted = append(wanted, WantedItem{
				TmdbID: m.TmdbID,
				ImdbID: m.ImdbID,
				Title:  m.Title,
				Year:   m.Year,
				Type:   "movie",
			})
		}
	}
	return wanted
}

// ExtractWantedSeries returns series that are monitored with missing episodes.
func ExtractWantedSeries(series []Series) []WantedItem {
	var wanted []WantedItem
	for _, s := range series {
		if !s.Monitored {
			continue
		}
		// Series with less than 100% of episodes downloaded
		if s.Statistics.EpisodeCount > 0 && s.Statistics.EpisodeFileCount < s.Statistics.EpisodeCount {
			id := s.ImdbID
			wanted = append(wanted, WantedItem{
				ImdbID: id,
				Title:  s.Title,
				Year:   s.Year,
				Type:   "show",
			})
		}
	}
	return wanted
}

// BuildMigrationResult aggregates all extracted data into a single result.
func BuildMigrationResult(
	movies []Movie,
	series []Series,
	radarrProfiles, sonarrProfiles []QualityProfile,
	radarrFolders, sonarrFolders []RootFolder,
	indexers []Indexer,
	downloadClients []DownloadClient,
) *MigrationResult {
	result := &MigrationResult{}

	// Stats
	result.TotalMovies = len(movies)
	for _, m := range movies {
		if m.HasFile {
			result.MoviesWithFiles++
		}
	}
	result.TotalSeries = len(series)
	for _, s := range series {
		if s.Statistics.EpisodeCount > 0 && s.Statistics.EpisodeFileCount >= s.Statistics.EpisodeCount {
			result.SeriesComplete++
		}
	}
	result.IndexerCount = len(indexers)

	for _, dc := range downloadClients {
		if dc.Enable {
			result.DownloadClients = append(result.DownloadClients, dc.ImplementationName)
		}
	}

	// Root folders → paths (uses most-popular folder based on item paths)
	result.MoviesDir, result.TVShowsDir = MapRootFolders(radarrFolders, sonarrFolders, movies, series)
	if result.MoviesDir != "" || result.TVShowsDir != "" {
		result.OrganizeEnabled = true
	}

	// Quality profile — use the most popular one across both apps
	profileCounts := map[int]int{}
	for _, m := range movies {
		profileCounts[m.QualityProfileID]++
	}
	for _, s := range series {
		profileCounts[s.QualityProfileID]++
	}
	allProfiles := make([]QualityProfile, 0, len(radarrProfiles)+len(sonarrProfiles))
	allProfiles = append(allProfiles, radarrProfiles...)
	allProfiles = append(allProfiles, sonarrProfiles...)
	if p := MostUsedProfile(profileCounts, allProfiles); p != nil {
		result.Quality = MapQualityProfile(*p)
		result.QualitySource = p.Name
	}

	// Wanted lists
	result.WantedMovies = ExtractWantedMovies(movies)
	result.WantedSeries = ExtractWantedSeries(series)

	return result
}

// ExtractBlocklistedHashes returns unique infoHashes from blocklist entries.
func ExtractBlocklistedHashes(items []BlocklistItem) []string {
	seen := map[string]bool{}
	var hashes []string
	for _, item := range items {
		h := strings.ToLower(strings.TrimSpace(item.Data.InfoHash))
		if h != "" && !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	return hashes
}

// ExtractDownloadedHashes returns unique infoHashes from history (imported items).
func ExtractDownloadedHashes(records []HistoryRecord) []string {
	seen := map[string]bool{}
	var hashes []string
	for _, r := range records {
		// Only count actually imported downloads, not just grabs
		if r.EventType != "downloadFolderImported" && r.EventType != "downloadImported" {
			continue
		}
		h := strings.ToLower(strings.TrimSpace(r.Data.InfoHash))
		if h != "" && !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	return hashes
}

// ExtractDebridTokens looks for debrid-related download clients and extracts tokens.
func ExtractDebridTokens(clients []DownloadClient, getFields func(id int) []Field) []DebridToken {
	debridKeywords := map[string]string{
		"realdebrid":  "real-debrid",
		"real-debrid": "real-debrid",
		"alldebrid":   "alldebrid",
		"torbox":      "torbox",
		"premiumize":  "premiumize",
	}

	var tokens []DebridToken
	for _, dc := range clients {
		if !dc.Enable {
			continue
		}
		impl := strings.ToLower(dc.Implementation + dc.ImplementationName)
		provider := ""
		for kw, prov := range debridKeywords {
			if strings.Contains(impl, kw) {
				provider = prov
				break
			}
		}
		if provider == "" {
			continue
		}

		// Get the fields for this download client to find the API key/token
		fields := getFields(dc.ID)
		for _, f := range fields {
			name := strings.ToLower(f.Name)
			if name == "apikey" || name == "api_key" || name == "token" || name == "apitoken" {
				if s, ok := f.Value.(string); ok && s != "" {
					tokens = append(tokens, DebridToken{
						Provider: provider,
						Token:    s,
						Name:     dc.Name,
					})
					break
				}
			}
		}
	}
	return tokens
}

// HasDockerPaths checks if any paths look like Docker container paths
// (e.g. /data, /movies, /tv) rather than real host paths.
func HasDockerPaths(result *MigrationResult) bool {
	dockerPrefixes := []string{"/data/", "/movies", "/tv", "/media", "/downloads"}
	for _, path := range []string{result.MoviesDir, result.TVShowsDir} {
		if path == "" {
			continue
		}
		for _, prefix := range dockerPrefixes {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
	}
	return false
}
