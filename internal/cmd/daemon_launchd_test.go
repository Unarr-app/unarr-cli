package cmd

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/service"
)

type launchdExitError int

func (e launchdExitError) Error() string { return "launchctl failed" }
func (e launchdExitError) ExitCode() int { return int(e) }

func TestLaunchdStartUsesExistingRegistration(t *testing.T) {
	var calls []string
	a := launchdAgent{domain: "gui/501", label: "test", settle: 0}
	a.run = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "state = running\n\tpid = 123\n", nil
	}
	if err := a.start(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls[:3], []string{"enable gui/501/test", "print gui/501/test", "kickstart gui/501/test"}) {
		t.Fatal(calls)
	}
	for _, call := range calls {
		if strings.Contains(call, "bootstrap") || strings.Contains(call, "-k") {
			t.Fatalf("restarted an existing process: %s", call)
		}
	}
}

func TestLaunchdStartBootstrapsOnlyAbsentService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	loaded := false
	a := launchdAgent{domain: "gui/501", label: "test", path: path}
	a.run = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "print":
			if !loaded {
				return "missing", launchdExitError(113)
			}
			return "state = running\npid = 123\n", nil
		case "bootstrap":
			loaded = true
		}
		return "", nil
	}
	if err := a.start(); err != nil {
		t.Fatal(err)
	}
	if calls[2] != "bootstrap gui/501 "+path {
		t.Fatal(calls)
	}
}

func TestLaunchdDoesNotIgnoreServiceManagerErrors(t *testing.T) {
	for _, operation := range []string{"enable", "print", "bootstrap", "kickstart", "bootout"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "job.plist")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			a := launchdAgent{domain: "gui/501", label: "test", path: path}
			a.run = func(args ...string) (string, error) {
				if args[0] == operation {
					return "permission denied", launchdExitError(5)
				}
				if operation == "bootstrap" && args[0] == "print" {
					return "absent", launchdExitError(113)
				}
				return "state = running\npid = 123\n", nil
			}
			var err error
			if operation == "bootout" {
				err = a.stop()
			} else {
				err = a.start()
			}
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("error swallowed: %v", err)
			}
		})
	}
}

func TestLaunchdStopWaitsForRegistrationToDisappear(t *testing.T) {
	queries := 0
	a := launchdAgent{domain: "gui/501", label: "test", timeout: time.Second}
	a.run = func(args ...string) (string, error) {
		if args[0] == "print" {
			queries++
			if queries >= 4 {
				return "", launchdExitError(113)
			}
		}
		return "state = waiting\n", nil
	}
	if err := a.stop(); err != nil {
		t.Fatal(err)
	}
	if queries != 4 {
		t.Fatalf("returned before bootout completed: %d queries", queries)
	}
}

func TestLaunchdStopTimeoutAndAlreadyStopped(t *testing.T) {
	a := launchdAgent{domain: "gui/501", label: "test"}
	a.run = func(...string) (string, error) { return "", nil }
	if err := a.stop(); err == nil {
		t.Fatal("expected timeout while still registered")
	}
	a.run = func(args ...string) (string, error) {
		if args[0] != "print" {
			t.Fatalf("mutated an absent job: %v", args)
		}
		return "absent", launchdExitError(113)
	}
	if err := a.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchdPlistEscapesPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.plist")
	data := serviceData{BinPath: "/Users/A & B/<bin>/unarr", LogDir: "/Users/A & B/logs"}
	if err := writeServiceFile(path, launchdTemplate, data); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := xml.NewDecoder(strings.NewReader(string(b)))
	found := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("invalid plist XML: %v", err)
		}
		if chars, ok := tok.(xml.CharData); ok && string(chars) == data.BinPath {
			found = true
		}
	}
	if !found {
		t.Fatal("executable path changed during XML escaping")
	}
}

func TestLegacyLaunchdRemovalPreservesUnknownFilesAndFailedStops(t *testing.T) {
	for _, label := range []string{service.LegacyLaunchdLabel, service.LaunchdLabel, "unrelated.agent"} {
		t.Run(label, func(t *testing.T) {
			home := t.TempDir()
			path := service.LegacyPlistPath(home)
			body := "<plist><dict><key>Label</key><string>" + label + "</string></dict></plist>"
			if err := writeServiceFile(path, body, serviceData{}); err != nil {
				t.Fatal(err)
			}
			a := &launchdAgent{domain: "gui/501", run: func(...string) (string, error) {
				return "denied", launchdExitError(1)
			}}
			if err := removeLegacyLaunchd(a, home); err == nil {
				t.Fatal("expected refusal")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("removed plist before stopping its job", err)
			}
			a.run = func(args ...string) (string, error) {
				if strings.Count(args[1], "/") == 1 {
					return "domain", nil
				}
				return "absent", launchdExitError(113)
			}
			err := removeLegacyLaunchd(a, home)
			if label == "unrelated.agent" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("removed unrelated file", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("old definition survived migration", err)
			}
		})
	}
}

func TestLaunchdDomainSelection(t *testing.T) {
	for _, tc := range []struct {
		name, existing, want string
		gui                  bool
	}{
		{"new desktop install", "", "gui/", true},
		{"SSH only", "", "user/", false},
		{"preserve headless registration", "user/", "user/", true},
		{"preserve GUI registration", "gui/", "gui/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := launchdAgent{label: "test"}
			a.run = func(args ...string) (string, error) {
				target := args[1]
				if strings.HasPrefix(target, "gui/") && !tc.gui {
					return "no session", launchdExitError(125)
				}
				if strings.Count(target, "/") == 1 || (tc.existing != "" && strings.HasPrefix(target, tc.existing)) {
					return "loaded", nil
				}
				return "missing", launchdExitError(113)
			}
			if err := a.findDomain(); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(a.domain, tc.want) {
				t.Fatalf("domain=%s, want %s", a.domain, tc.want)
			}
		})
	}
}

func TestRemoveRedundantPlistDoesNotStopHealthyService(t *testing.T) {
	home := t.TempDir()
	path := service.LegacyPlistPath(home)
	if err := writeServiceFile(path, launchdTemplate, serviceData{}); err != nil {
		t.Fatal(err)
	}
	a := &launchdAgent{label: service.LaunchdLabel, path: service.PlistPath(home)}
	a.run = func(args ...string) (string, error) {
		t.Fatalf("redundant filename must not affect live job: %v", args)
		return "", nil
	}
	if err := removeLegacyLaunchd(a, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("redundant plist survived", err)
	}
}
