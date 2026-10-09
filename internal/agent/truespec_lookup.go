package agent

import (
	"context"
	"fmt"
	"strings"
)

// TrueSpecLookupResult is TorrentClaw's TrueSpec state for one infohash.
type TrueSpecLookupResult struct {
	InfoHash string `json:"infohash"`
	Verified bool   `json:"verified"`
	Mismatch bool   `json:"mismatch"`
}

type trueSpecLookupRequest struct {
	Prefixes []string `json:"prefixes"`
}

type trueSpecLookupResponse struct {
	Results []TrueSpecLookupResult `json:"results"`
}

// TrueSpecLookup asks which torrents starting with each hex prefix TorrentClaw
// has scanned (POST /api/v1/truespec/lookup). Only short prefixes leave the
// machine (k-anonymity): the server rejects anything that is not exactly 5 hex
// characters, so a full infohash can never be sent by mistake.
func (c *Client) TrueSpecLookup(ctx context.Context, prefixes []string) ([]TrueSpecLookupResult, error) {
	for _, p := range prefixes {
		if len(p) != 5 || strings.Trim(strings.ToLower(p), "0123456789abcdef") != "" {
			return nil, fmt.Errorf("truespec lookup: %q is not a 5-hex-character prefix", p)
		}
	}
	var resp trueSpecLookupResponse
	if err := c.doPost(ctx, "/api/v1/truespec/lookup", trueSpecLookupRequest{Prefixes: prefixes}, &resp); err != nil {
		return nil, fmt.Errorf("truespec lookup: %w", err)
	}
	return resp.Results, nil
}
