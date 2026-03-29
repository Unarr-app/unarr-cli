package arr

// SystemStatus is returned by GET /api/v{n}/system/status.
type SystemStatus struct {
	AppName     string `json:"appName"`
	Version     string `json:"version"`
	StartupPath string `json:"startupPath"`
}

// QualityProfile represents a quality configuration in Sonarr/Radarr.
type QualityProfile struct {
	ID     int           `json:"id"`
	Name   string        `json:"name"`
	Items  []QualityItem `json:"items"`
	Cutoff int           `json:"cutoff"`
}

// QualityItem is a single entry (or group) inside a quality profile.
type QualityItem struct {
	Quality *Quality      `json:"quality"`
	Items   []QualityItem `json:"items"` // nested quality groups
	Allowed bool          `json:"allowed"`
}

// Quality describes a single quality level.
type Quality struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Resolution int    `json:"resolution"`
}

// RootFolder is a media root folder configured in Sonarr/Radarr.
type RootFolder struct {
	ID        int    `json:"id"`
	Path      string `json:"path"`
	FreeSpace int64  `json:"freeSpace"`
}

// Movie is a Radarr movie record from GET /api/v3/movie.
type Movie struct {
	ID               int    `json:"id"`
	TmdbID           int    `json:"tmdbId"`
	ImdbID           string `json:"imdbId"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	Path             string `json:"path"`
	RootFolderPath   string `json:"rootFolderPath"`
	QualityProfileID int    `json:"qualityProfileId"`
	Monitored        bool   `json:"monitored"`
	HasFile          bool   `json:"hasFile"`
	SizeOnDisk       int64  `json:"sizeOnDisk"`
}

// Series is a Sonarr series record from GET /api/v3/series.
type Series struct {
	ID               int              `json:"id"`
	TvdbID           int              `json:"tvdbId"`
	ImdbID           string           `json:"imdbId"`
	Title            string           `json:"title"`
	Year             int              `json:"year"`
	Path             string           `json:"path"`
	RootFolderPath   string           `json:"rootFolderPath"`
	QualityProfileID int              `json:"qualityProfileId"`
	Monitored        bool             `json:"monitored"`
	Statistics       SeriesStatistics `json:"statistics"`
}

// SeriesStatistics holds episode-level stats for a series.
type SeriesStatistics struct {
	EpisodeCount      int     `json:"episodeCount"`
	EpisodeFileCount  int     `json:"episodeFileCount"`
	SizeOnDisk        int64   `json:"sizeOnDisk"`
	PercentOfEpisodes float64 `json:"percentOfEpisodes"`
}

// Indexer is a Prowlarr indexer from GET /api/v1/indexer.
type Indexer struct {
	ID                 int    `json:"id"`
	Name               string `json:"name"`
	Enable             bool   `json:"enable"`
	ImplementationName string `json:"implementationName"`
}

// Application is a Prowlarr-connected app from GET /api/v1/applications.
type Application struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
}

// Field is a dynamic key-value pair used in Prowlarr indexer/app configs.
type Field struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// DownloadClient is a download client configured in Sonarr/Radarr.
type DownloadClient struct {
	ID                 int    `json:"id"`
	Name               string `json:"name"`
	Enable             bool   `json:"enable"`
	Protocol           string `json:"protocol"` // "torrent" or "usenet"
	Implementation     string `json:"implementation"`
	ImplementationName string `json:"implementationName"`
}

// Tag is a label applied to movies/series in Sonarr/Radarr.
type Tag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

// HistoryRecord is a single entry from /api/v3/history.
type HistoryRecord struct {
	ID           int    `json:"id"`
	EventType    string `json:"eventType"` // "grabbed", "downloadFolderImported", etc.
	DownloadID   string `json:"downloadId"`
	SourceTitle  string `json:"sourceTitle"`
	Data         HistoryData `json:"data"`
}

// HistoryData holds the nested data of a history record.
type HistoryData struct {
	InfoHash    string `json:"torrentInfoHash"`
	DownloadURL string `json:"downloadUrl"`
}

// HistoryResponse wraps the paginated history from *arr.
type HistoryResponse struct {
	Records []HistoryRecord `json:"records"`
	TotalRecords int        `json:"totalRecords"`
}

// BlocklistItem is an item the user explicitly rejected.
type BlocklistItem struct {
	ID          int    `json:"id"`
	SourceTitle string `json:"sourceTitle"`
	Data        BlocklistData `json:"data"`
}

// BlocklistData holds torrent info from a blocklist entry.
type BlocklistData struct {
	InfoHash string `json:"torrentInfoHash"`
}

// BlocklistResponse wraps paginated blocklist from *arr.
type BlocklistResponse struct {
	Records []BlocklistItem `json:"records"`
	TotalRecords int        `json:"totalRecords"`
}

// Instance represents a discovered *arr application.
type Instance struct {
	App     string // "sonarr", "radarr", "prowlarr"
	URL     string // "http://localhost:8989"
	APIKey  string // from config.xml or user input
	Version string // from /system/status
	Source  string // "docker", "port-scan", "config-file", "prowlarr", "manual"
}

// MigrationResult holds the mapped data ready to apply.
type MigrationResult struct {
	// Config changes
	MoviesDir       string
	TVShowsDir      string
	Quality         string // "2160p", "1080p", "720p"
	QualitySource   string // name of the quality profile used
	OrganizeEnabled bool

	// Wanted list
	WantedMovies []WantedItem
	WantedSeries []WantedItem

	// Exclusions
	BlocklistedHashes []string // infoHashes the user has rejected
	DownloadedHashes  []string // infoHashes already downloaded (from history)

	// Debrid
	DebridTokens []DebridToken // tokens extracted from *arr download clients

	// Media servers
	MediaServers []string // detected media servers (e.g. "Plex at localhost:32400")

	// Stats (informational)
	TotalMovies     int
	MoviesWithFiles int
	TotalSeries     int
	SeriesComplete  int
	IndexerCount    int
	DownloadClients []string // names of detected download clients
}

// DebridToken is a debrid API token extracted from an *arr download client.
type DebridToken struct {
	Provider string // "real-debrid", "alldebrid", "torbox", "premiumize"
	Token    string
	Name     string // download client name from *arr
}

// WantedItem is a movie or series the user wants but doesn't have yet.
type WantedItem struct {
	TmdbID int    `json:"tmdbId,omitempty"`
	ImdbID string `json:"imdbId,omitempty"`
	Title  string `json:"title"`
	Year   int    `json:"year,omitempty"`
	Type   string `json:"type"` // "movie" or "show"
}
