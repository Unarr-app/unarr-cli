//go:build windows

package main

// Windows self-registration of the unarr:// URL scheme. HKCU (per-user) needs
// no admin rights and is what browsers consult for protocol handlers:
//
//	HKCU\Software\Classes\unarr                     (Default) = "URL:unarr"
//	HKCU\Software\Classes\unarr  "URL Protocol"     = ""      (marker value)
//	HKCU\Software\Classes\unarr\shell\open\command  (Default) = "<exe>" --open "%1"
//
// Running this on every tray start (instead of only from install.ps1) is
// deliberate: it self-heals after the binary moves (updater, reinstall to a
// different dir) without installer cooperation, and the same-value check keeps
// it a cheap no-op read on the steady state.

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const schemeKeyPath = `Software\Classes\unarr`

// registerURLScheme idempotently claims the unarr:// scheme for THIS
// executable. Failures only log to stderr — a broken registry write must
// never take the tray down (the handler is a convenience, not a dependency).
func registerURLScheme() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: register scheme: resolve executable:", err)
		return
	}
	command := `"` + exe + `" --open "%1"`
	if currentSchemeCommand() == command {
		return // already registered for this exact binary — nothing to write
	}
	if err := writeSchemeKeys(command); err != nil {
		fmt.Fprintln(os.Stderr, "unarr-desktop: register scheme:", err)
	}
}

// currentSchemeCommand returns the registered open command, or "" when the
// scheme is absent/unreadable (either way: proceed to write).
func currentSchemeCommand() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, schemeKeyPath+`\shell\open\command`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("")
	if err != nil {
		return ""
	}
	return v
}

func writeSchemeKeys(command string) error {
	root, _, err := registry.CreateKey(registry.CURRENT_USER, schemeKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create %s: %w", schemeKeyPath, err)
	}
	defer root.Close()
	if err := root.SetStringValue("", "URL:unarr"); err != nil {
		return fmt.Errorf("set default value: %w", err)
	}
	// The empty "URL Protocol" value is the marker Windows requires to treat
	// the class as a URL scheme at all.
	if err := root.SetStringValue("URL Protocol", ""); err != nil {
		return fmt.Errorf("set URL Protocol marker: %w", err)
	}
	cmdKey, _, err := registry.CreateKey(registry.CURRENT_USER, schemeKeyPath+`\shell\open\command`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create command key: %w", err)
	}
	defer cmdKey.Close()
	if err := cmdKey.SetStringValue("", command); err != nil {
		return fmt.Errorf("set command: %w", err)
	}
	return nil
}
