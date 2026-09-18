package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/service"
	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

type limitedOutput struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if n > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// Only stdout is collected. Error messages and stderr can contain identities,
// paths, environment values and credentials, and never enter the report.
func localCommand(limit int, name string, args ...string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	winproc.HideWindow(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	out := &limitedOutput{limit: limit}
	cmd.Stdout = out
	err := cmd.Run()
	return out.Bytes(), out.truncated, err
}

func operatingSystemVersion() string {
	var b []byte
	var err error
	switch runtime.GOOS {
	case "darwin":
		b, _, err = localCommand(64, "/usr/bin/sw_vers", "-productVersion")
	case "linux", "freebsd":
		b, _, err = localCommand(128, "/usr/bin/uname", "-r")
	default:
		return "unknown"
	}
	if err != nil {
		return "unknown"
	}
	value := strings.TrimSpace(string(b))
	// Kernel vendor suffixes are free text, so retain only the dotted release.
	if i := strings.IndexByte(value, '-'); i >= 0 {
		value = value[:i]
	}
	return safeVersion(value, numericVersion)
}

func collectService() Service {
	s := Service{Manager: "unknown", State: "unknown", DaemonState: "unknown"}
	switch runtime.GOOS {
	case "darwin":
		s.Manager = "launchd"
		collectLaunchdService(&s)
	case "linux":
		s.Manager = "systemd"
		b, _, err := localCommand(4096, "systemctl", "--user", "show", service.SystemdUnitName, "--property=LoadState,ActiveState,ExecMainStatus")
		if err == nil {
			parseSystemdService(&s, string(b))
		}
	case "windows":
		s.Manager = "windows-task"
	default:
		s.Manager = "none"
	}
	return s
}

func collectLaunchdService(s *Service) {
	absent := true
	for _, domain := range []string{"gui/", "user/"} {
		for _, label := range []string{service.LaunchdLabel, service.LegacyLaunchdLabel} {
			b, _, err := localCommand(64*1024, "/bin/launchctl", "print", domain+strconv.Itoa(os.Getuid())+"/"+label)
			if err == nil {
				parseLaunchdService(s, string(b))
				return
			}
			var exit interface{ ExitCode() int }
			if !errors.As(err, &exit) || (exit.ExitCode() != 112 && exit.ExitCode() != 113 && exit.ExitCode() != 125) {
				absent = false
			}
		}
	}
	if absent {
		s.State = "stopped"
	}
}

func exitCode(value string) *int {
	n, err := strconv.Atoi(value)
	if err != nil || n < -255 || n > 255 {
		return nil
	}
	return &n
}

func parseLaunchdService(s *Service, out string) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			switch value {
			case "running":
				s.State = "running"
			case "waiting", "exited", "not running":
				s.State = "stopped"
			}
		case "last exit code":
			s.LastExitCode = exitCode(value)
		}
	}
	if s.State != "running" && s.LastExitCode != nil && *s.LastExitCode != 0 {
		s.State = "failed"
	}
}

func parseSystemdService(s *Service, out string) {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			switch value {
			case "active":
				s.State = "running"
			case "inactive":
				s.State = "stopped"
			case "failed":
				s.State = "failed"
			}
		case "ExecMainStatus":
			s.LastExitCode = exitCode(value)
		}
	}
}
