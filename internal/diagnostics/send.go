package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const MaxReportBytes = 1 << 20
const reportEndpoint = "https://unarr.app/api/internal/agent/diagnostic-report"

// Set with release ldflags only AFTER the complete deployed request path has
// been verified not to retain IPs. Even a capability GET must not hit an
// unverified proxy. Local collection/save remain available in every build.
var privateDeliveryEnabled = "false"

var ErrUnavailable = errors.New("private report delivery is unavailable; keep the local report")

// Send uploads precisely the reviewed bytes. Call only after explicit consent.
// This transport deliberately does not use the authenticated agent client,
// environment proxies, cookies, redirects, mirrors, or telemetry.
func Send(ctx context.Context, payload []byte) error {
	if privateDeliveryEnabled != "true" {
		return ErrUnavailable
	}
	client, transport := reportHTTPClient()
	defer transport.CloseIdleConnections()
	return send(ctx, client, reportEndpoint, payload)
}

func reportHTTPClient() (*http.Client, *http.Transport) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Transport:     transport,
		Timeout:       20 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, transport
}

func send(ctx context.Context, client *http.Client, endpoint string, payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxReportBytes {
		return ErrUnavailable
	}
	body, err := requestReportJSON(ctx, client, endpoint, nil)
	if err != nil {
		return ErrUnavailable
	}
	var capability struct {
		SchemaVersion int  `json:"schemaVersion"`
		Accepts       bool `json:"acceptsAnonymousReports"`
		MaxBytes      int  `json:"maxBytes"`
	}
	if json.Unmarshal(body, &capability) != nil || capability.SchemaVersion != 1 ||
		!capability.Accepts || capability.MaxBytes < len(payload) {
		return ErrUnavailable
	}
	body, err = requestReportJSON(ctx, client, endpoint, payload)
	if err != nil {
		return ErrUnavailable
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(body, &result) != nil || !result.OK {
		return ErrUnavailable
	}
	return nil
}

func requestReportJSON(ctx context.Context, client *http.Client, endpoint string, payload []byte) ([]byte, error) {
	method := http.MethodGet
	if payload != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("User-Agent", "unarr-diagnostics/1")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if resp.StatusCode != http.StatusOK || err != nil || len(body) > 4096 {
		return nil, ErrUnavailable
	}
	return body, nil
}
