package arr

import (
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
