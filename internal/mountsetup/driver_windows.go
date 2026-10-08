package mountsetup

import (
	"context"
	"debug/pe"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
	"golang.org/x/sys/windows/registry"
)

func activateDriver(context.Context, Options) error { return nil }

func runWindowsInstaller(ctx context.Context, installer string) error {
	if code := legacyWinFspProduct(); code != "" {
		if err := runWindowsMSI(ctx, `/x `+code+` /passive /norestart`); err != nil {
			return err
		}
	}
	return runWindowsMSI(ctx, `/i "`+installer+`" /passive /norestart`)
}

func runWindowsMSI(ctx context.Context, args string) error {
	// Prefer a deferred reboot to Restart Manager closing unrelated applications
	// (or our own controller when WinFsp's network provider is loaded into it).
	args += " MSIRESTARTMANAGERCONTROL=Disable"
	// RunAs preserves the native UAC consent/credential prompt. Passive MSI UI
	// removes redundant Next/Finish clicks after our explained approval.
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
	file, err := pe.Open(filepath.Join(dir, "bin", name))
	if err != nil {
		return false
	}
	defer file.Close()
	machine := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
	if runtime.GOARCH == "arm64" {
		machine = pe.IMAGE_FILE_MACHINE_ARM64
	}
	return file.Machine == machine && file.Characteristics&pe.IMAGE_FILE_DLL != 0
}
