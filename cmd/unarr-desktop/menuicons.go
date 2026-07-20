package main

// Per-item menu icons. The set is generated from Lucide (scripts/gen-menu-icons)
// into two variants per item: a TEMPLATE icon (black + alpha, which macOS treats
// as a mask and recolors to match a light/dark menu) and a REGULAR icon (light
// gray, the fallback the systray backend uses on Linux/Windows). SetTemplateIcon
// takes both, so one call covers every platform. Icons are cosmetic: a missing
// asset just renders the row without one, never an error.

import (
	"embed"
	"fmt"

	"fyne.io/systray"
)

//go:embed menuicons/*.png
var menuIconFS embed.FS

// menuIcon returns the template + regular PNG bytes for a menu-item icon name
// (nil if the asset is absent).
func menuIcon(name string) (template, regular []byte) {
	template, _ = menuIconFS.ReadFile(fmt.Sprintf("menuicons/%s_template.png", name))
	regular, _ = menuIconFS.ReadFile(fmt.Sprintf("menuicons/%s_regular.png", name))
	return template, regular
}

// setMenuIcon applies an embedded icon to a menu item (no-op when the item or
// the asset is missing).
func setMenuIcon(item *systray.MenuItem, name string) {
	if item == nil {
		return
	}
	t, r := menuIcon(name)
	if len(r) == 0 {
		return
	}
	item.SetTemplateIcon(t, r)
}

// applyMenuIcons decorates the menu rows with their icons. The autostart row is
// a checkbox (its own checkmark) and the Player submenu entries are checkboxes
// too, so they stay icon-free to avoid clashing with the check glyph.
func (ui *trayUI) applyMenuIcons() {
	icons := map[*systray.MenuItem]string{
		ui.mStatus:          "status",
		ui.mAccount:         "account",
		ui.mVersion:         "version",
		ui.mUpgrade:         "upgrade",
		ui.mPause:           "pause",
		ui.mResume:          "resume",
		ui.mRestart:         "restart",
		ui.mEnableDownloads: "enable",
		ui.mOpen:            "open",
		ui.mLibrary:         "library",
		ui.mDownloads:       "downloads",
		ui.mConfigure:       "configure",
		ui.mEdit:            "edit",
		ui.mPlayer:          "player",
		ui.mLogs:            "logs",
		ui.mSendLogs:        "sendlogs",
		ui.mDocs:            "docs",
		ui.mUpdate:          "update",
		ui.mQuit:            "quit",
	}
	for item, name := range icons {
		setMenuIcon(item, name)
	}
}
