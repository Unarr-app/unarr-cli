package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/fatih/color"
)

func TestLaunchdUnknownLegacyDoesNotBlockHealthyStart(t *testing.T) {
	for _, body := range []string{"broken XML", "<plist><dict><key>Label</key><string>unrelated.agent</string></dict></plist>"} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			path := service.LegacyPlistPath(home)
			if err := writeServiceFile(path, "{{.BinPath}}", serviceData{BinPath: body}); err != nil {
				t.Fatal(err)
			}
			a := &launchdAgent{label: service.LaunchdLabel, path: service.PlistPath(home), domain: "gui/501"}
			a.run = func(args ...string) (string, error) {
				if args[0] == "print" {
					if strings.Count(args[1], "/") == 1 {
						return "domain", nil
					}
					if strings.HasSuffix(args[1], "/"+service.LaunchdLabel) {
						return "state = running\npid = 123\n", nil
					}
					return "missing", launchdExitError(113)
				}
				if args[0] != "enable" && args[0] != "kickstart" {
					t.Fatalf("healthy service changed: %v", args)
				}
				return "", nil
			}
			if err := removeLegacyLaunchd(a, home); err != nil {
				t.Fatal(err)
			}
			if err := a.start(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != body {
				t.Fatalf("unknown file changed: %q %v", got, err)
			}
		})
	}
}

func TestLaunchdInstallRollbackDependsOnRegistration(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		previous, registered, unknown bool
	}{
		{"fresh rejected bootstrap", false, false, false},
		{"restore old definition", true, false, false},
		{"preserve registered job", false, true, false},
		{"preserve unknown registration", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := service.PlistPath(home)
			original := "original definition with {{literal}}"
			if tc.previous {
				if err := writeServiceFile(path, "{{.BinPath}}", serviceData{BinPath: original}); err != nil {
					t.Fatal(err)
				}
			}
			attempted := false
			a := &launchdAgent{timeout: time.Second}
			a.run = func(args ...string) (string, error) {
				switch args[0] {
				case "print":
					if strings.Count(args[1], "/") == 1 {
						return "domain", nil
					}
					if attempted && strings.HasSuffix(args[1], "/"+service.LaunchdLabel) {
						if tc.unknown {
							return "permission denied", launchdExitError(1)
						}
						if tc.registered {
							return "state = waiting\n", nil
						}
					}
					return "absent", launchdExitError(113)
				case "bootstrap":
					attempted = true
					return "bootstrap rejected", launchdExitError(5)
				case "enable":
					return "", nil
				default:
					t.Fatalf("unexpected operation: %v", args)
					return "", nil
				}
			}
			data := serviceData{Home: home, BinPath: "/fixture/unarr", LogDir: filepath.Join(home, "logs")}
			err := installLaunchdWithAgent(data, color.New(color.FgGreen), a)
			if err == nil || !strings.Contains(err.Error(), "bootstrap rejected") {
				t.Fatalf("missing install failure: %v", err)
			}
			got, readErr := os.ReadFile(path)
			if tc.registered || tc.unknown {
				if readErr != nil || !strings.Contains(string(got), data.BinPath) {
					t.Fatalf("lost registered/unknown definition: %v", readErr)
				}
			} else if tc.previous {
				if readErr != nil || string(got) != original {
					t.Fatalf("did not restore exact definition: %q %v", got, readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("phantom supervisor survived: %v", readErr)
			}
		})
	}
}

func TestLaunchdInstallPreservesUnrecognizedLegacy(t *testing.T) {
	home := t.TempDir()
	legacy := service.LegacyPlistPath(home)
	if err := writeServiceFile(legacy, "damaged", serviceData{}); err != nil {
		t.Fatal(err)
	}
	loaded := true
	bootstrapped := false
	a := &launchdAgent{timeout: time.Second}
	a.run = func(args ...string) (string, error) {
		switch args[0] {
		case "print":
			if strings.Count(args[1], "/") == 1 {
				return "domain", nil
			}
			if loaded && strings.HasSuffix(args[1], "/"+service.LaunchdLabel) {
				if bootstrapped {
					return "state = running\npid = 123\n", nil
				}
				return "state = running\n", nil
			}
			return "absent", launchdExitError(113)
		case "bootout":
			loaded = false
		case "bootstrap":
			loaded, bootstrapped = true, true
		}
		return "", nil
	}
	// Supply a PID only after bootstrap, so stop tests registration without
	// checking or signaling any real process on the host.
	if err := installLaunchdWithAgent(serviceData{Home: home, BinPath: "/fixture/unarr", LogDir: filepath.Join(home, "logs")}, color.New(color.FgGreen), a); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(legacy)
	if err != nil || string(got) != "damaged" {
		t.Fatalf("legacy changed: %q %v", got, err)
	}
}
