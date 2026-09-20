package mountsetup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
	"golang.org/x/sys/windows/registry"
)

func runWindowsInstaller(ctx context.Context, installer string) error {
	// RunAs preserves the native UAC consent/credential prompt. Passive MSI UI
	// removes redundant Next/Finish clicks after our explained approval.
	args := `/i "` + installer + `" /passive /norestart`
	script := `$ErrorActionPreference='Stop'; $p=Start-Process -FilePath 'msiexec.exe' -ArgumentList '` + strings.ReplaceAll(args, "'", "''") + `' -Verb RunAs -Wait -PassThru; exit $p.ExitCode`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	winproc.HideWindow(cmd)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("WinFsp installation did not complete: %w", err)
	}
	return nil
}

func driverReady() error {
	for _, access := range []uint32{registry.READ | registry.WOW64_32KEY, registry.READ | registry.WOW64_64KEY} {
		key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, access)
		if err != nil {
			continue
		}
		dir, _, err := key.GetStringValue("InstallDir")
		_ = key.Close()
		if err == nil && hasWinFspDLL(dir) {
			return nil
		}
	}
	for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles")} {
		if root != "" && hasWinFspDLL(filepath.Join(root, "WinFsp")) {
			return nil
		}
	}
	return errDriverMissing
}

func hasWinFspDLL(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	name := "winfsp-x64.dll"
	if runtime.GOARCH == "arm64" {
		name = "winfsp-a64.dll"
	}
	info, err := os.Stat(filepath.Join(dir, "bin", name))
	return err == nil && info.Mode().IsRegular()
}
