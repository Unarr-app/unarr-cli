package mediaserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePlexSections(t *testing.T) {
	body := `{
		"MediaContainer": {
			"Directory": [
				{
					"title": "Movies",
					"Location": [{"path": "/data/media/movies"}]
				},
				{
					"title": "TV Shows",
					"Location": [{"path": "/data/media/tv"}]
				}
			]
		}
	}`

	paths := parsePlexSections([]byte(body))
	if len(paths) != 2 {
		t.Fatalf("parsePlexSections = %d paths, want 2", len(paths))
	}
	if paths[0] != "/data/media/movies" {
		t.Errorf("paths[0] = %q, want /data/media/movies", paths[0])
	}
	if paths[1] != "/data/media/tv" {
		t.Errorf("paths[1] = %q, want /data/media/tv", paths[1])
	}
}

func TestParsePlexSections_Empty(t *testing.T) {
	paths := parsePlexSections([]byte(`{}`))
	if len(paths) != 0 {
		t.Errorf("parsePlexSections empty = %d paths, want 0", len(paths))
	}
}

func TestParsePlexSections_InvalidJSON(t *testing.T) {
	paths := parsePlexSections([]byte(`not json`))
	if paths != nil {
		t.Errorf("parsePlexSections invalid = %v, want nil", paths)
	}
}

func TestJellyfinParsing(t *testing.T) {
	body := `[
		{"Locations": ["/media/movies"]},
		{"Locations": ["/media/tv", "/media/anime"]}
	]`

	var folders []struct {
		Locations []string `json:"Locations"`
	}
	if err := json.Unmarshal([]byte(body), &folders); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for _, f := range folders {
		paths = append(paths, f.Locations...)
	}
	if len(paths) != 3 {
		t.Fatalf("got %d paths, want 3", len(paths))
	}
}

func TestPlexTokenFromPrefs(t *testing.T) {
	t.Run("valid prefs", func(t *testing.T) {
		dir := t.TempDir()
		prefsPath := filepath.Join(dir, "Preferences.xml")
		xml := `<?xml version="1.0" encoding="utf-8"?>
<Preferences PlexOnlineToken="my-secret-token" OldestPreviousVersion="1.0"/>`
		os.WriteFile(prefsPath, []byte(xml), 0o644)

		token := plexTokenFromPrefs(prefsPath)
		if token != "my-secret-token" {
			t.Errorf("token = %q, want my-secret-token", token)
		}
	})

	t.Run("no token attr", func(t *testing.T) {
		dir := t.TempDir()
		prefsPath := filepath.Join(dir, "Preferences.xml")
		xml := `<?xml version="1.0"?><Preferences/>`
		os.WriteFile(prefsPath, []byte(xml), 0o644)

		token := plexTokenFromPrefs(prefsPath)
		if token != "" {
			t.Errorf("token = %q, want empty", token)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		token := plexTokenFromPrefs("/nonexistent/Preferences.xml")
		if token != "" {
			t.Errorf("token = %q, want empty", token)
		}
	})

	t.Run("invalid xml", func(t *testing.T) {
		dir := t.TempDir()
		prefsPath := filepath.Join(dir, "Preferences.xml")
		os.WriteFile(prefsPath, []byte("not xml at all"), 0o644)

		token := plexTokenFromPrefs(prefsPath)
		if token != "" {
			t.Errorf("token = %q, want empty", token)
		}
	})
}

func TestParsePlexSectionsMultipleLocations(t *testing.T) {
	body := `{
		"MediaContainer": {
			"Directory": [
				{
					"title": "Movies",
					"Location": [
						{"path": "/media/movies"},
						{"path": "/media/movies2"}
					]
				}
			]
		}
	}`

	paths := parsePlexSections([]byte(body))
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
}

func TestParsePlexSectionsEmptyPath(t *testing.T) {
	body := `{
		"MediaContainer": {
			"Directory": [
				{
					"Location": [{"path": ""}, {"path": "/valid"}]
				}
			]
		}
	}`

	paths := parsePlexSections([]byte(body))
	if len(paths) != 1 {
		t.Fatalf("expected 1 path (empty filtered), got %d: %v", len(paths), paths)
	}
}

func TestCommonMediaDirs(t *testing.T) {
	dirs := commonMediaDirs()
	if len(dirs) == 0 {
		t.Error("expected at least some common media dirs")
	}
}

func TestParentDir(t *testing.T) {
	tests := []struct {
		name   string
		paths  []string
		expect string
	}{
		{"empty", nil, ""},
		{"single", []string{"/data/media/movies"}, "/data/media"},
		{"siblings", []string{"/data/media/movies", "/data/media/tv"}, "/data/media"},
		{"different roots", []string{"/data/movies", "/srv/tv"}, "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParentDir(tt.paths)
			// "/" is filtered out (returns "")
			if tt.expect == "/" {
				if got != "" {
					t.Errorf("ParentDir = %q, want empty (root filtered)", got)
				}
				return
			}
			if got != tt.expect {
				t.Errorf("ParentDir = %q, want %q", got, tt.expect)
			}
		})
	}
}
