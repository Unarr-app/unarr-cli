package cmd

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"

	"github.com/Unarr-app/unarr-cli/internal/service"
)

var errUnknownLegacyPlist = errors.New("unrecognized launchd plist")

func stopLaunchdServices(a *launchdAgent, home string) error {
	canonical := *a
	canonical.label, canonical.path = service.LaunchdLabel, service.PlistPath(home)
	if err := canonical.findDomain(); err != nil {
		return err
	}
	if err := canonical.stop(); err != nil {
		return err
	}
	return stopLegacyLaunchd(&canonical, home)
}

// Retire only the known old daemon definition, after its job has stopped. Never
// sweep LaunchAgents by substring: the desktop's login agent is independent.
func removeLegacyLaunchd(a *launchdAgent, home string) error {
	_, inspectErr := legacyPlistLabel(service.LegacyPlistPath(home))
	if inspectErr != nil && !errors.Is(inspectErr, errUnknownLegacyPlist) {
		return inspectErr
	}
	if err := stopLegacyLaunchd(a, home); err != nil {
		return err
	}
	if errors.Is(inspectErr, errUnknownLegacyPlist) {
		return nil // Preserve files whose ownership cannot be established.
	}
	if err := os.Remove(service.LegacyPlistPath(home)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old launchd plist: %w", err)
	}
	return nil
}

func stopLegacyLaunchd(a *launchdAgent, home string) error {
	legacy := *a
	legacy.label, legacy.path = service.LegacyLaunchdLabel, service.LegacyPlistPath(home)
	label, err := legacyPlistLabel(legacy.path)
	if errors.Is(err, errUnknownLegacyPlist) {
		// Disk contents cannot hide an independently registered known job.
		label, err = service.LegacyLaunchdLabel, nil
	}
	if err != nil {
		return err
	}
	legacy.label = label
	if legacy.label == a.label && legacy.path != a.path {
		// Two filenames for the SAME job cannot compete. Start must not stop
		// the healthy canonical process merely to remove the redundant file.
		return nil
	}
	if err := legacy.findDomain(); err != nil {
		return err
	}
	return legacy.stop()
}

func legacyPlistLabel(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return service.LegacyLaunchdLabel, nil
	}
	if err != nil {
		return "", err
	}
	// Decode the flat dictionary so a similarly named, unrelated plist is never
	// removed. Accept either supported label under the legacy filename.
	label, err := launchdPlistLabel(b)
	if err == nil && (label == service.LegacyLaunchdLabel || label == service.LaunchdLabel) {
		return label, nil
	}
	return "", fmt.Errorf("%w %s; leaving it intact", errUnknownLegacyPlist, path)
}

// Restore only after proving that a failed start never registered the new job.
// Unknown status is deliberately not grounds for removing a supervisor's file.
func rollbackLaunchdDefinition(a *launchdAgent, previous []byte, existed bool) error {
	_, loaded, err := a.status()
	if err != nil || loaded {
		return err
	}
	if existed {
		return writeServiceFile(a.path, "{{.BinPath}}", serviceData{BinPath: string(previous)})
	}
	if err := os.Remove(a.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func launchdPlistLabel(b []byte) (string, error) {
	var plist struct {
		Dict struct {
			Entries []struct {
				XMLName xml.Name
				Text    string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(b, &plist); err != nil {
		return "", err
	}
	entries := plist.Dict.Entries
	for i := 0; i+1 < len(entries); i++ {
		if entries[i].XMLName.Local == "key" && entries[i].Text == "Label" && entries[i+1].XMLName.Local == "string" {
			return entries[i+1].Text, nil
		}
	}
	return "", fmt.Errorf("missing launchd Label")
}
