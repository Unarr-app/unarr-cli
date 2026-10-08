//go:build darwin

package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/service"
)

func nativeMacCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func nativeMacRealHome(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil || u.Uid != strconv.Itoa(os.Getuid()) || os.Getuid() == 0 {
		t.Fatalf("current non-root user identity required: %v %v", u, err)
	}
	name, err := nativeMacCommand("/usr/bin/id", "-un")
	if err != nil || strings.TrimSpace(string(name)) != u.Username {
		t.Fatalf("native username mismatch: %v %q", err, name)
	}
	out, err := nativeMacCommand("/usr/bin/dscl", ".", "-read", "/Users/"+u.Username, "NFSHomeDirectory")
	if err != nil {
		t.Fatalf("authoritative DirectoryServices home: %v %s", err, out)
	}
	home := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "NFSHomeDirectory:"))
	if !filepath.IsAbs(home) || home != u.HomeDir {
		t.Fatalf("real home mismatch: DirectoryServices=%q os/user=%q", home, u.HomeDir)
	}
	return home
}

func nativeMacAbsent(t *testing.T, home string) {
	t.Helper()
	for _, p := range []string{service.PlistPath(home), service.LegacyPlistPath(home)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("refusing existing/inaccessible real plist %s: %v", p, err)
		}
	}
	for _, kind := range []string{"gui/", "user/"} {
		domain := kind + strconv.Itoa(os.Getuid())
		if _, err := launchctlOutput("print", domain); err != nil {
			if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 112 {
				continue
			}
			t.Fatalf("inaccessible real domain %s: %v", domain, err)
		}
		for _, label := range []string{service.LaunchdLabel, service.LegacyLaunchdLabel} {
			a := launchdAgent{domain: domain, label: label, run: launchctlOutput}
			if _, loaded, err := a.status(); err != nil || loaded {
				t.Fatalf("real label not absent %s: loaded=%t err=%v", a.target(), loaded, err)
			}
		}
	}
}

func nativeMacPrivatePath(t *testing.T, home, p string) {
	t.Helper()
	rel, err := filepath.Rel(home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("path escaped fixture: %s", p)
	}
	for cursor := p; ; cursor = filepath.Dir(cursor) {
		info, err := os.Lstat(cursor)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("unowned/symlink fixture path: %s", cursor)
			}
		}
		if cursor == home {
			break
		}
	}
}

func nativeMacProcess(pid int, executable string) (nativeMacIdentity, error) {
	out, err := nativeMacCommand("/bin/ps", "-ww", "-p", strconv.Itoa(pid), "-o", "lstart=", "-o", "comm=")
	receipt := strings.TrimSpace(string(out))
	if err != nil {
		return nativeMacIdentity{}, err
	}
	if !strings.HasSuffix(receipt, " "+executable) {
		return nativeMacIdentity{}, fmt.Errorf("PID%d executable mismatch: %q expected %q", pid, receipt, executable)
	}
	return nativeMacIdentity{pid: pid, receipt: receipt}, nil
}

func (id nativeMacIdentity) gone() bool {
	out, err := nativeMacCommand("/bin/ps", "-ww", "-p", strconv.Itoa(id.pid), "-o", "lstart=", "-o", "comm=")
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
		return true
	}
	return err == nil && strings.TrimSpace(string(out)) != id.receipt // A reused PID is not the recorded process.
}

func (g *nativeMacPersistent) owned() error {
	info, err := os.Lstat("/usr/bin/sandbox-exec")
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, g.sandboxExecInfo) {
		return fmt.Errorf("system sandbox-exec identity changed; refuse service mutator: %v", err)
	}
	data, err := os.ReadFile("/usr/bin/sandbox-exec")
	if err != nil || sha256.Sum256(data) != g.sandboxExecHash {
		return fmt.Errorf("system sandbox-exec content changed; refuse service mutator: %v", err)
	}
	info, err = os.Lstat(g.sandbox)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, g.sandboxInfo) {
		return fmt.Errorf("private sandbox inode replaced; preserve it: %v", err)
	}
	data, err = os.ReadFile(g.sandbox)
	if err != nil || !bytes.Equal(data, g.sandboxDefinition) {
		return fmt.Errorf("private sandbox definition replaced; preserve it: %v", err)
	}
	for _, p := range []string{service.PlistPath(g.realHome), service.LegacyPlistPath(g.realHome)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return fmt.Errorf("real plist appeared: %s", p)
		}
	}
	info, err = os.Lstat(g.plist)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, g.definitionInfo) {
		return fmt.Errorf("private definition inode replaced; preserve it: %v", err)
	}
	data, err = os.ReadFile(g.plist)
	if err != nil || !bytes.Equal(data, g.definition) {
		return fmt.Errorf("private definition absent/replaced; preserve it: %v", err)
	}
	for _, kind := range []string{"gui/", "user/"} {
		domain := kind + strconv.Itoa(os.Getuid())
		if _, err := launchctlOutput("print", domain); err != nil {
			if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 112 {
				continue
			}
			return err
		}
		for _, label := range []string{service.LaunchdLabel, service.LegacyLaunchdLabel} {
			a := launchdAgent{domain: domain, label: label, run: launchctlOutput}
			out, loaded, err := a.status()
			if err != nil {
				return err
			}
			if !loaded {
				continue
			}
			if !g.launched || label != service.LaunchdLabel || domain != g.agent.domain {
				return fmt.Errorf("foreign launchd owner %s", a.target())
			}
			fields := map[string]string{}
			for _, line := range strings.Split(out, "\n") {
				if k, v, ok := strings.Cut(strings.TrimSpace(line), " = "); ok {
					fields[k] = v
				}
			}
			program := g.cli
			if g.sandboxed {
				program = "/usr/bin/sandbox-exec"
			}
			if fields["path"] != g.plist || fields["program"] != program || !strings.Contains(out, "HOME => "+g.home+"\n") {
				return fmt.Errorf("loaded definition/program/environment not owned: %s %s", fields["path"], fields["program"])
			}
			pid := launchdPID(out)
			if pid != 0 {
				if _, err := nativeMacProcess(pid, g.cli); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
