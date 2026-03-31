package arr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, handlers map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check API key header
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		handler, ok := handlers[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(handler)
	}))
}

func TestNewClient(t *testing.T) {
	c := NewClient("http://localhost:8989/", "mykey")
	if c.baseURL != "http://localhost:8989" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
	if c.apiKey != "mykey" {
		t.Errorf("apiKey = %q, want mykey", c.apiKey)
	}
}

func TestSystemStatus(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v3/system/status": SystemStatus{AppName: "Radarr", Version: "4.0.0"},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	status, err := c.SystemStatus()
	if err != nil {
		t.Fatalf("SystemStatus: %v", err)
	}
	if status.AppName != "Radarr" {
		t.Errorf("AppName = %q, want Radarr", status.AppName)
	}
	if status.Version != "4.0.0" {
		t.Errorf("Version = %q, want 4.0.0", status.Version)
	}
}

func TestSystemStatusFallbackV1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/system/status":
			w.WriteHeader(http.StatusNotFound)
		case "/api/v1/system/status":
			json.NewEncoder(w).Encode(SystemStatus{AppName: "Prowlarr", Version: "1.0.0"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	status, err := c.SystemStatus()
	if err != nil {
		t.Fatalf("SystemStatus v1 fallback: %v", err)
	}
	if status.AppName != "Prowlarr" {
		t.Errorf("AppName = %q, want Prowlarr", status.AppName)
	}
}

func TestMovies(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v3/movie": []Movie{
			{ID: 1, Title: "Inception", Year: 2010, TmdbID: 27205, Monitored: true},
			{ID: 2, Title: "Tenet", Year: 2020, TmdbID: 577922, HasFile: true},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	movies, err := c.Movies()
	if err != nil {
		t.Fatalf("Movies: %v", err)
	}
	if len(movies) != 2 {
		t.Fatalf("expected 2 movies, got %d", len(movies))
	}
	if movies[0].Title != "Inception" {
		t.Errorf("movies[0].Title = %q, want Inception", movies[0].Title)
	}
}

func TestSeries(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v3/series": []Series{
			{ID: 1, Title: "Breaking Bad", Year: 2008, TvdbID: 81189},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	series, err := c.Series()
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	if series[0].Title != "Breaking Bad" {
		t.Errorf("series[0].Title = %q, want Breaking Bad", series[0].Title)
	}
}

func TestQualityProfiles(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v3/qualityprofile": []QualityProfile{
			{ID: 1, Name: "HD-1080p"},
			{ID: 2, Name: "Ultra-HD"},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	profiles, err := c.QualityProfiles()
	if err != nil {
		t.Fatalf("QualityProfiles: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}
}

func TestRootFolders(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v3/rootfolder": []RootFolder{
			{ID: 1, Path: "/movies", FreeSpace: 500000000000},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	folders, err := c.RootFolders()
	if err != nil {
		t.Fatalf("RootFolders: %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("expected 1 folder, got %d", len(folders))
	}
	if folders[0].Path != "/movies" {
		t.Errorf("path = %q, want /movies", folders[0].Path)
	}
}

func TestDownloadClients(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v3/downloadclient": []DownloadClient{
			{ID: 1, Name: "Transmission", Enable: true, Protocol: "torrent"},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	clients, err := c.DownloadClients()
	if err != nil {
		t.Fatalf("DownloadClients: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "Transmission" {
		t.Errorf("unexpected clients: %+v", clients)
	}
}

func TestDownloadClientDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v3/downloadclient/5" {
			json.NewEncoder(w).Encode(struct {
				Fields []Field `json:"fields"`
			}{
				Fields: []Field{
					{Name: "host", Value: "localhost"},
					{Name: "port", Value: 9091},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	fields, err := c.DownloadClientDetails(5)
	if err != nil {
		t.Fatalf("DownloadClientDetails: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
	if fields[0].Name != "host" {
		t.Errorf("fields[0].Name = %q, want host", fields[0].Name)
	}
}

func TestTags(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v3/tag": []Tag{
			{ID: 1, Label: "unarr"},
			{ID: 2, Label: "imported"},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	tags, err := c.Tags()
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(tags))
	}
}

func TestHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v3/history" {
			json.NewEncoder(w).Encode(HistoryResponse{
				Records: []HistoryRecord{
					{ID: 1, EventType: "grabbed", SourceTitle: "Inception.2010.1080p"},
				},
				TotalRecords: 1,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	records, err := c.History(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].SourceTitle != "Inception.2010.1080p" {
		t.Errorf("sourceTitle = %q", records[0].SourceTitle)
	}
}

func TestHistoryDefaultPageSize(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		requestedPath = r.URL.String()
		json.NewEncoder(w).Encode(HistoryResponse{})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	c.History(0) // should default to 250

	if requestedPath == "" {
		t.Fatal("no request made")
	}
	if !contains(requestedPath, "pageSize=250") {
		t.Errorf("expected pageSize=250, got path: %s", requestedPath)
	}
}

func TestBlocklist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v3/blocklist" {
			json.NewEncoder(w).Encode(BlocklistResponse{
				Records: []BlocklistItem{
					{ID: 1, SourceTitle: "Bad.Release", Data: BlocklistData{InfoHash: "abc123"}},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	items, err := c.Blocklist(50)
	if err != nil {
		t.Fatalf("Blocklist: %v", err)
	}
	if len(items) != 1 || items[0].Data.InfoHash != "abc123" {
		t.Errorf("unexpected blocklist: %+v", items)
	}
}

func TestIndexers(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v1/indexer": []Indexer{
			{ID: 1, Name: "NZBGeek", Enable: true},
			{ID: 2, Name: "Torznab", Enable: false},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	indexers, err := c.Indexers()
	if err != nil {
		t.Fatalf("Indexers: %v", err)
	}
	if len(indexers) != 2 {
		t.Fatalf("expected 2 indexers, got %d", len(indexers))
	}
}

func TestApplications(t *testing.T) {
	srv := newTestServer(t, map[string]any{
		"/api/v1/applications": []Application{
			{ID: 1, Name: "Radarr"},
		},
	})
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	apps, err := c.Applications()
	if err != nil {
		t.Fatalf("Applications: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "Radarr" {
		t.Errorf("unexpected apps: %+v", apps)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := newTestServer(t, map[string]any{})
	defer srv.Close()

	c := NewClient(srv.URL, "wrong-key")
	_, err := c.SystemStatus()
	if err == nil {
		t.Error("expected error for unauthorized request")
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	_, err := c.Movies()
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
