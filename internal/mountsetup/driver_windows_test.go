package mountsetup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWinFspPartialAndWrongArchitecture(t *testing.T) {
	for _, mode := range []string{"absent", "truncated", "wrong-architecture", "executable-not-dll", "valid-header"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			name, machine := "winfsp-x64.dll", uint16(pe.IMAGE_FILE_MACHINE_AMD64)
			if runtime.GOARCH == "arm64" {
				name, machine = "winfsp-a64.dll", pe.IMAGE_FILE_MACHINE_ARM64
			}
			if mode == "wrong-architecture" {
				machine = pe.IMAGE_FILE_MACHINE_I386
			}
			flags := uint16(pe.IMAGE_FILE_DLL)
			if mode == "executable-not-dll" {
				flags = 0
			}
			var buf bytes.Buffer
			if err := binary.Write(&buf, binary.LittleEndian, pe.FileHeader{Machine: machine, Characteristics: flags}); err != nil {
				t.Fatal(err)
			}
			// debug/pe reads a DOS-header-sized prefix even for a bare COFF file.
			data := append(buf.Bytes(), make([]byte, 128)...)
			if mode == "truncated" {
				data = []byte("MZ")
			}
			if mode != "absent" {
				if err := os.WriteFile(filepath.Join(dir, "bin", name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := hasWinFspDLL(dir); got != (mode == "valid-header") {
				t.Fatalf("%s: present=%v", mode, got)
			}
		})
	}
	if hasWinFspDLL("relative") {
		t.Fatal("relative registry path accepted")
	}
}

func TestLegacyWinFspSelection(t *testing.T) {
	code := "{11111111-2222-3333-4444-555555555555}"
	for _, tc := range []struct {
		name, version, id string
		want              bool
	}{
		{"WinFsp 2022", "1.12.22301", code, true},
		{"WinFsp 2025", "2.1.25156", code, false},
		{"Other application", "1.12", code, false},
		{"WinFsp 2022", "1.12", "anything.exe", false},
		{"WinFsp 2022", "1.12", code + " & other", false},
	} {
		if got := isLegacyWinFsp(tc.name, tc.version, tc.id); got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
}

func TestNativeWindowsInstaller(t *testing.T) {
	path := os.Getenv("UNARR_TEST_WINDOWS_MSI")
	if path == "" {
		t.Skip("explicit verified MSI path and test-machine authorization required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != winfspArtifact.SHA256 {
		t.Fatal("unverified installer")
	}
	// Match production's local verified download. The elevated MSI service cannot
	// necessarily access a VM's unauthenticated UNC share as the interactive user.
	path = filepath.Join(t.TempDir(), "winfsp.msi")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Log("TEST APPROVAL:", driverExplanation("windows"))
	err = runWindowsInstaller(context.Background(), path)
	if os.Getenv("UNARR_TEST_REBOOT_REQUIRED") == "1" {
		var status *exec.ExitError
		if !errors.As(err, &status) || status.ExitCode() != 3010 {
			t.Fatalf("expected deferred reboot, got %v", err)
		}
		t.Log("REBOOT_REQUIRED: controller survived; installation stopped until operator restart")
		return
	}
	if os.Getenv("UNARR_TEST_UAC_CANCEL") == "1" {
		if err == nil {
			t.Fatal("expected native UAC cancellation")
		}
		t.Log("Native cancellation returned an error:", err)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := driverReady(); err != nil {
		t.Fatal(err)
	}
}
