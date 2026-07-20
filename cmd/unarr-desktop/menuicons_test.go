package main

import (
	"bytes"
	"image/png"
	"testing"
)

// Every menu-item icon name must resolve to a decodable template + regular PNG
// so a missing/renamed asset fails the build's test step, not silently at
// runtime (where a broken icon just vanishes and is easy to miss).
func TestMenuIconsEmbedded(t *testing.T) {
	names := []string{
		"status", "account", "version", "upgrade", "pause", "resume", "restart",
		"enable", "open", "library", "downloads", "configure", "edit", "player",
		"logs", "sendlogs", "docs", "update", "quit",
	}
	for _, name := range names {
		tmpl, reg := menuIcon(name)
		for variant, data := range map[string][]byte{"template": tmpl, "regular": reg} {
			if len(data) == 0 {
				t.Errorf("%s (%s): missing embedded icon", name, variant)
				continue
			}
			if _, err := png.Decode(bytes.NewReader(data)); err != nil {
				t.Errorf("%s (%s): not a decodable PNG: %v", name, variant, err)
			}
		}
	}
}
