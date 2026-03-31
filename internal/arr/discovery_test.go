package arr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseConfigXML(t *testing.T) {
	xml := `<Config>
		<Port>8989</Port>
		<ApiKey>abc123def456</ApiKey>
		<UrlBase>/sonarr</UrlBase>
	</Config>`

	port, apiKey, urlBase := parseConfigXML(strings.NewReader(xml))
	if port != "8989" {
		t.Errorf("port = %q, want 8989", port)
	}
	if apiKey != "abc123def456" {
		t.Errorf("apiKey = %q, want abc123def456", apiKey)
	}
	if urlBase != "/sonarr" {
		t.Errorf("urlBase = %q, want /sonarr", urlBase)
	}
}

func TestParseConfigXML_Minimal(t *testing.T) {
	xml := `<Config><Port>7878</Port><ApiKey>key</ApiKey></Config>`

	port, apiKey, urlBase := parseConfigXML(strings.NewReader(xml))
	if port != "7878" || apiKey != "key" || urlBase != "" {
		t.Errorf("got port=%q apiKey=%q urlBase=%q", port, apiKey, urlBase)
	}
}

func TestParseConfigXML_Invalid(t *testing.T) {
	port, apiKey, _ := parseConfigXML(strings.NewReader("not xml"))
	if port != "" || apiKey != "" {
		t.Errorf("invalid XML should return empty values")
	}
}

func TestExtractHostPort(t *testing.T) {
	tests := []struct {
		ports     string
		container string
		want      string
	}{
		{"0.0.0.0:8989->8989/tcp", "8989", "8989"},
		{"0.0.0.0:9090->8989/tcp, :::9090->8989/tcp", "8989", "9090"},
		{"0.0.0.0:7878->7878/tcp", "7878", "7878"},
		{"", "8989", ""},
		{"0.0.0.0:3000->3000/tcp", "8989", ""},
	}
	for _, tt := range tests {
		t.Run(tt.ports, func(t *testing.T) {
			got := extractHostPort(tt.ports, tt.container)
			if got != tt.want {
				t.Errorf("extractHostPort(%q, %q) = %q, want %q", tt.ports, tt.container, got, tt.want)
			}
		})
	}
}

func TestDetectApp(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"linuxserver/sonarr:latest", "sonarr"},
		{"hotio/radarr", "radarr"},
		{"ghcr.io/linuxserver/prowlarr:develop", "prowlarr"},
		{"nginx:latest", ""},
		{"postgres:16", ""},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			got := detectApp(tt.image)
			if got != tt.want {
				t.Errorf("detectApp(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestConfigDirs(t *testing.T) {
	dirs := configDirs()
	if len(dirs) == 0 {
		t.Error("configDirs() returned empty")
	}
}

func TestParseConfigXMLEmpty(t *testing.T) {
	port, apiKey, urlBase := parseConfigXML(strings.NewReader(""))
	if port != "" || apiKey != "" || urlBase != "" {
		t.Error("empty input should return empty values")
	}
}

func TestParseConfigXMLNoPort(t *testing.T) {
	xml := `<Config><ApiKey>key123</ApiKey></Config>`
	port, apiKey, _ := parseConfigXML(strings.NewReader(xml))
	if port != "" {
		t.Errorf("port = %q, want empty", port)
	}
	if apiKey != "key123" {
		t.Errorf("apiKey = %q, want key123", apiKey)
	}
}

func TestExtractHostPortMultipleMappings(t *testing.T) {
	tests := []struct {
		name      string
		ports     string
		container string
		want      string
	}{
		{"ipv6 only", ":::8989->8989/tcp", "8989", "8989"},
		{"different host port", "0.0.0.0:9999->8989/tcp", "8989", "9999"},
		{"port in string but no mapping", "something 8989 somewhere", "8989", "8989"},
		{"no match at all", "0.0.0.0:3000->3000/tcp", "9999", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHostPort(tt.ports, tt.container)
			if got != tt.want {
				t.Errorf("extractHostPort(%q, %q) = %q, want %q", tt.ports, tt.container, got, tt.want)
			}
		})
	}
}

func TestDiscoverFromProwlarr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			json.NewEncoder(w).Encode([]Application{
				{
					ID:   1,
					Name: "Radarr",
					Fields: []Field{
						{Name: "baseUrl", Value: "http://localhost:7878"},
						{Name: "apiKey", Value: "radarr-key-123"},
					},
				},
				{
					ID:   2,
					Name: "Sonarr",
					Fields: []Field{
						{Name: "baseUrl", Value: "http://localhost:8989"},
						{Name: "apiKey", Value: "sonarr-key-456"},
					},
				},
				{
					ID:   3,
					Name: "Unknown App",
					Fields: []Field{
						{Name: "baseUrl", Value: "http://localhost:9000"},
						{Name: "apiKey", Value: "unknown-key"},
					},
				},
				{
					ID:   4,
					Name: "Incomplete",
					Fields: []Field{
						{Name: "baseUrl", Value: "http://localhost:5000"},
						// no apiKey → should be skipped
					},
				},
			})
		case "/api/v3/system/status":
			json.NewEncoder(w).Encode(SystemStatus{AppName: "Radarr", Version: "4.0.0"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// DiscoverFromProwlarr will try to verify each instance, which will fail
	// for localhost URLs (not our test server), but that's OK — we test the parsing
	instances := DiscoverFromProwlarr(srv.URL, "prowlarr-key")

	// Should find Radarr and Sonarr (Unknown and Incomplete skipped)
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d: %+v", len(instances), instances)
	}

	found := map[string]bool{}
	for _, inst := range instances {
		found[inst.App] = true
		if inst.Source != "prowlarr" {
			t.Errorf("source = %q, want prowlarr", inst.Source)
		}
	}
	if !found["radarr"] {
		t.Error("expected radarr instance")
	}
	if !found["sonarr"] {
		t.Error("expected sonarr instance")
	}
}

func TestVerify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "valid-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(SystemStatus{AppName: "Radarr", Version: "5.0.0"})
	}))
	defer srv.Close()

	t.Run("valid", func(t *testing.T) {
		inst := &Instance{App: "radarr", URL: srv.URL, APIKey: "valid-key"}
		err := Verify(inst)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if inst.Version != "5.0.0" {
			t.Errorf("version = %q, want 5.0.0", inst.Version)
		}
	})

	t.Run("no api key", func(t *testing.T) {
		inst := &Instance{App: "radarr", URL: srv.URL}
		err := Verify(inst)
		if err == nil {
			t.Error("expected error for no API key")
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		inst := &Instance{App: "radarr", URL: srv.URL, APIKey: "wrong-key"}
		err := Verify(inst)
		if err == nil {
			t.Error("expected error for invalid API key")
		}
	})
}
