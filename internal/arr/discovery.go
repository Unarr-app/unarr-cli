package arr

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// appInfo maps app names to their default ports and API versions.
var appInfo = map[string]struct {
	Port    string
	Version string
}{
	"sonarr":   {Port: "8989", Version: "v3"},
	"radarr":   {Port: "7878", Version: "v3"},
	"prowlarr": {Port: "9696", Version: "v1"},
}

// Discover scans for running *arr instances using multiple strategies.
// Returns instances in order: Docker, config files, port scan.
func Discover() []Instance {
	seen := map[string]bool{} // dedupe by URL
	var instances []Instance

	add := func(inst Instance) {
		key := strings.ToLower(inst.URL)
		if seen[key] {
			// Allow upgrading a no-key entry with one that has a key
			if inst.APIKey != "" {
				for i := range instances {
					if strings.ToLower(instances[i].URL) == key && instances[i].APIKey == "" {
						instances[i].APIKey = inst.APIKey
						instances[i].Source = inst.Source
						if inst.Version != "" {
							instances[i].Version = inst.Version
						}
						break
					}
				}
			}
			return
		}
		seen[key] = true
		instances = append(instances, inst)
	}

	// Strategy 1: Docker containers
	for _, inst := range discoverDocker() {
		add(inst)
	}

	// Strategy 2: Config files on disk
	for _, inst := range discoverConfigFiles() {
		add(inst)
	}

	// Strategy 3: Port scan on localhost
	for _, inst := range discoverPorts() {
		add(inst)
	}

	return instances
}

// ── Docker discovery ────────────────────────────────────────────────

func discoverDocker() []Instance {
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		return nil
	}

	out, err := exec.Command(dockerPath, "ps", "--format", "{{.Names}}\t{{.Image}}\t{{.Ports}}").Output()
	if err != nil {
		return nil
	}

	var instances []Instance
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		name, image, ports := parts[0], strings.ToLower(parts[1]), parts[2]

		app := detectApp(image)
		if app == "" {
			continue
		}

		port := extractHostPort(ports, appInfo[app].Port)
		if port == "" {
			continue
		}

		url := "http://localhost:" + port

		// Try to read API key from container's config.xml
		apiKey := readDockerConfigXML(dockerPath, name)

		inst := Instance{
			App:    app,
			URL:    url,
			APIKey: apiKey,
			Source: "docker",
		}

		// Verify connectivity if we have an API key
		if apiKey != "" {
			if status, err := NewClient(url, apiKey).SystemStatus(); err == nil {
				inst.Version = status.Version
			}
		}

		instances = append(instances, inst)
	}
	return instances
}

func detectApp(image string) string {
	for _, app := range []string{"sonarr", "radarr", "prowlarr"} {
		if strings.Contains(image, app) {
			return app
		}
	}
	return ""
}

// extractHostPort finds the host port mapped to the expected container port.
func extractHostPort(portsStr, containerPort string) string {
	// Format: "0.0.0.0:8989->8989/tcp, ..."
	for _, mapping := range strings.Split(portsStr, ",") {
		mapping = strings.TrimSpace(mapping)
		if strings.Contains(mapping, "->"+containerPort+"/") {
			parts := strings.SplitN(mapping, "->", 2)
			if len(parts) == 2 {
				hostPart := parts[0]
				// Remove IP prefix: "0.0.0.0:8989" → "8989"
				if idx := strings.LastIndex(hostPart, ":"); idx >= 0 {
					return hostPart[idx+1:]
				}
				return hostPart
			}
		}
	}
	// Fallback: check if the expected port appears at all
	if strings.Contains(portsStr, containerPort) {
		return containerPort
	}
	return ""
}

func readDockerConfigXML(dockerPath, containerName string) string {
	out, err := exec.Command(dockerPath, "exec", containerName, "cat", "/config/config.xml").Output()
	if err != nil {
		return ""
	}
	_, apiKey, _ := parseConfigXML(bytes.NewReader(out))
	return apiKey
}

// ── Config file discovery ───────────────────────────────────────────

func discoverConfigFiles() []Instance {
	var instances []Instance

	dirs := configDirs()
	for _, app := range []string{"Sonarr", "Radarr", "Prowlarr"} {
		for _, dir := range dirs {
			cfgPath := filepath.Join(dir, app, "config.xml")
			f, err := os.Open(cfgPath)
			if err != nil {
				continue
			}

			port, apiKey, urlBase := parseConfigXML(f)
			_ = f.Close()

			if port == "" || apiKey == "" {
				continue
			}

			url := "http://localhost:" + port
			if urlBase != "" {
				url += "/" + strings.Trim(urlBase, "/")
			}

			inst := Instance{
				App:    strings.ToLower(app),
				URL:    url,
				APIKey: apiKey,
				Source: "config-file",
			}

			if status, err := NewClient(url, apiKey).SystemStatus(); err == nil {
				inst.Version = status.Version
			}

			instances = append(instances, inst)
		}
	}
	return instances
}

func configDirs() []string {
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("PROGRAMDATA")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return []string{pd}
	default: // linux, darwin
		home, _ := os.UserHomeDir()
		return []string{
			filepath.Join(home, ".config"),
			"/var/lib",
		}
	}
}

// ── Port scan discovery ─────────────────────────────────────────────

func discoverPorts() []Instance {
	var instances []Instance

	// Fixed order iteration (map iteration is random in Go)
	portOrder := []string{"sonarr", "radarr", "prowlarr"}
	for _, app := range portOrder {
		info := appInfo[app]
		addr := "localhost:" + info.Port
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			continue
		}
		_ = conn.Close()

		url := "http://localhost:" + info.Port
		inst := Instance{
			App:    app,
			URL:    url,
			Source: "port-scan",
		}

		// Try to get status without API key (some configs allow local access)
		if status, err := NewClient(url, "").SystemStatus(); err == nil {
			inst.Version = status.Version
		}

		instances = append(instances, inst)
	}
	return instances
}

// ── Prowlarr application discovery ──────────────────────────────────

// DiscoverFromProwlarr extracts connected Sonarr/Radarr instances from Prowlarr.
func DiscoverFromProwlarr(prowlarrURL, prowlarrKey string) []Instance {
	client := NewClient(prowlarrURL, prowlarrKey)
	apps, err := client.Applications()
	if err != nil {
		return nil
	}

	var instances []Instance
	for _, app := range apps {
		var baseURL, apiKey string
		for _, f := range app.Fields {
			switch f.Name {
			case "baseUrl", "BaseUrl":
				if s, ok := f.Value.(string); ok {
					baseURL = s
				}
			case "apiKey", "ApiKey":
				if s, ok := f.Value.(string); ok {
					apiKey = s
				}
			}
		}
		if baseURL == "" || apiKey == "" {
			continue
		}

		appName := strings.ToLower(app.Name)
		detectedApp := ""
		for _, a := range []string{"sonarr", "radarr"} {
			if strings.Contains(appName, a) {
				detectedApp = a
				break
			}
		}
		if detectedApp == "" {
			continue
		}

		inst := Instance{
			App:    detectedApp,
			URL:    strings.TrimRight(baseURL, "/"),
			APIKey: apiKey,
			Source: "prowlarr",
		}

		if status, err := NewClient(inst.URL, apiKey).SystemStatus(); err == nil {
			inst.Version = status.Version
		}

		instances = append(instances, inst)
	}
	return instances
}

// ── Config XML parser ───────────────────────────────────────────────

type xmlConfig struct {
	XMLName xml.Name `xml:"Config"`
	Port    string   `xml:"Port"`
	APIKey  string   `xml:"ApiKey"`
	URLBase string   `xml:"UrlBase"`
}

func parseConfigXML(r io.Reader) (port, apiKey, urlBase string) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20)) // 1MB
	if err != nil {
		return "", "", ""
	}
	var cfg xmlConfig
	if err := xml.Unmarshal(data, &cfg); err != nil {
		return "", "", ""
	}
	return cfg.Port, cfg.APIKey, cfg.URLBase
}

// Verify checks that an instance is reachable and the API key is valid.
// Returns the system status on success.
func Verify(inst *Instance) error {
	if inst.APIKey == "" {
		return fmt.Errorf("no API key")
	}
	status, err := NewClient(inst.URL, inst.APIKey).SystemStatus()
	if err != nil {
		return err
	}
	inst.Version = status.Version
	return nil
}
