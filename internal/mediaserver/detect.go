package mediaserver

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Server represents a detected media server.
type Server struct {
	Name string // "Plex", "Jellyfin", "Emby"
	URL  string // "http://localhost:32400"
}

// DetectedPaths holds media library paths discovered from servers and disk.
type DetectedPaths struct {
	Servers []Server
	Paths   []string // unique media library paths found
}

var knownServers = []struct {
	Name string
	Port string
}{
	{"Plex", "32400"},
	{"Jellyfin", "8096"},
	{"Emby", "8920"},
}

// Detect scans for media servers and common media directories.
func Detect() DetectedPaths {
	result := DetectedPaths{}
	pathSet := map[string]bool{}

	addPath := func(p string) {
		p = filepath.Clean(p)
		if !pathSet[p] {
			pathSet[p] = true
			result.Paths = append(result.Paths, p)
		}
	}

	// 1. Detect media servers via port scan
	for _, s := range knownServers {
		conn, err := net.DialTimeout("tcp", "localhost:"+s.Port, 2*time.Second)
		if err != nil {
			continue
		}
		_ = conn.Close()
		result.Servers = append(result.Servers, Server{
			Name: s.Name,
			URL:  "http://localhost:" + s.Port,
		})
	}

	// 2. Try to read Plex library paths from config
	for _, p := range plexLibraryPaths() {
		addPath(p)
	}

	// 3. Try Jellyfin API (often allows local access without auth)
	for _, s := range result.Servers {
		if s.Name != "Jellyfin" {
			continue
		}
		for _, p := range jellyfinLibraryPaths(s.URL) {
			addPath(p)
		}
	}

	// 4. Scan common media directories on disk
	for _, p := range commonMediaDirs() {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			addPath(p)
		}
	}

	return result
}

// ── Plex ────────────────────────────────────────────────────────────

func plexLibraryPaths() []string {
	configDir := plexConfigDir()
	if configDir == "" {
		return nil
	}

	// Read token from Preferences.xml
	prefsPath := filepath.Join(configDir, "Preferences.xml")
	token := plexTokenFromPrefs(prefsPath)
	if token == "" {
		return nil
	}

	// Query library sections
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", "http://localhost:32400/library/sections", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}

	return parsePlexSections(body)
}

func plexConfigDir() string {
	switch runtime.GOOS {
	case "linux":
		home, _ := os.UserHomeDir()
		candidates := []string{
			filepath.Join(home, ".config", "Plex Media Server"),
			"/var/lib/plexmediaserver/Library/Application Support/Plex Media Server",
		}
		for _, d := range candidates {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				return d
			}
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Plex Media Server")
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "Plex Media Server")
	}
	return ""
}

type plexPrefs struct {
	XMLName         xml.Name `xml:"Preferences"`
	PlexOnlineToken string   `xml:"PlexOnlineToken,attr"`
}

func plexTokenFromPrefs(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var prefs plexPrefs
	if err := xml.Unmarshal(data, &prefs); err != nil {
		return ""
	}
	return prefs.PlexOnlineToken
}

func parsePlexSections(body []byte) []string {
	// Plex JSON response has: MediaContainer.Directory[].Location[].path
	var container struct {
		MediaContainer struct {
			Directory []struct {
				Location []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &container); err != nil {
		return nil
	}

	var paths []string
	for _, dir := range container.MediaContainer.Directory {
		for _, loc := range dir.Location {
			if loc.Path != "" {
				paths = append(paths, loc.Path)
			}
		}
	}
	return paths
}

// ── Jellyfin ────────────────────────────────────────────────────────

func jellyfinLibraryPaths(baseURL string) []string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/Library/VirtualFolders")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}

	var folders []struct {
		Locations []string `json:"Locations"`
	}
	if err := json.Unmarshal(body, &folders); err != nil {
		return nil
	}

	var paths []string
	for _, f := range folders {
		paths = append(paths, f.Locations...)
	}
	return paths
}

// ── Common directories ──────────────────────────────────────────────

func commonMediaDirs() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}

	candidates := []string{
		filepath.Join(home, "Media"),
		filepath.Join(home, "Movies"),
		filepath.Join(home, "Videos"),
		filepath.Join(home, "TV Shows"),
	}

	// Also check /data/media pattern (common Docker/NAS setup)
	if runtime.GOOS == "linux" {
		candidates = append(candidates,
			"/data/media",
			"/data/media/movies",
			"/data/media/tv",
			"/srv/media",
		)
	}

	return candidates
}

// ParentDir returns the common parent of detected paths, useful for
// suggesting a download directory that encompasses movie + TV paths.
func ParentDir(paths []string) string {
	if len(paths) == 0 {
		return ""
	}

	// Find the common prefix of all paths
	parent := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		d := filepath.Dir(p)
		for parent != "/" && parent != "." {
			if d == parent || strings.HasPrefix(d, parent+string(filepath.Separator)) {
				break
			}
			parent = filepath.Dir(parent)
		}
	}

	// Don't return root or home as a suggestion
	home, _ := os.UserHomeDir()
	if parent == "/" || parent == "." || parent == home {
		return ""
	}
	return parent
}
