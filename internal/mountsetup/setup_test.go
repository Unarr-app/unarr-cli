package mountsetup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDriverApprovalPrecedesInstallation(t *testing.T) {
	for _, mode := range []string{"installed", "unattended", "declined", "approved", "restart"} {
		t.Run(mode, func(t *testing.T) {
			installed, confirmed, calls := mode == "installed", false, 0
			ready := func() error {
				if installed {
					return nil
				}
				return errDriverMissing
			}
			opts := Options{Output: io.Discard}
			if mode != "unattended" {
				opts.Confirm = func(explanation string) error {
					if !strings.Contains(explanation, "needed") || !strings.Contains(explanation, "administrator") {
						t.Fatal("missing explanation", explanation)
					}
					confirmed = true
					if mode == "declined" {
						return errors.New("declined")
					}
					return nil
				}
			}
			err := prepareDriver(context.Background(), opts, ready, func(context.Context, Options) error {
				if !confirmed {
					t.Fatal("installed without prior explanation and approval")
				}
				calls++
				installed = mode != "restart"
				return nil
			})
			wantError := mode == "unattended" || mode == "declined" || mode == "restart"
			if (err != nil) != wantError {
				t.Fatalf("unexpected result: %v", err)
			}
			if (calls == 1) != (mode == "approved" || mode == "restart") {
				t.Fatalf("installation count %d", calls)
			}
			if mode == "installed" && confirmed {
				t.Fatal("prompted for an existing dependency")
			}
		})
	}
}

func TestPinnedArtifactsCoverReleasePlatforms(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			a, err := rcloneArtifact(goos, arch)
			if err != nil || len(a.SHA256) != 64 || !strings.HasPrefix(a.URL, "https://downloads.rclone.org/v") {
				t.Fatal(a, err)
			}
		}
	}
	if _, err := rcloneArtifact("plan9", "amd64"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
}

func TestDownloadRejectsBadBytesAndCancellation(t *testing.T) {
	data := []byte("verified release bytes")
	hash := sha256.Sum256(data)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	for _, mode := range []string{"valid", "corrupt", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			a := artifact{URL: server.URL, SHA256: hex.EncodeToString(hash[:])}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "corrupt" {
				a.SHA256 = strings.Repeat("0", 64)
			}
			if mode == "cancelled" {
				cancel()
			}
			path, err := download(ctx, server.Client(), a, dir)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				got, _ := os.ReadFile(path)
				if !bytes.Equal(got, data) {
					t.Fatal("wrong bytes")
				}
			} else {
				if err == nil {
					t.Fatal("unverified download accepted")
				}
				entries, _ := os.ReadDir(dir)
				if len(entries) != 0 {
					t.Fatal("partial download left behind")
				}
			}
		})
	}
}

func TestExtractionOnlyInstallsExactRegularMember(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "symlink", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "archive.zip")
			f, _ := os.Create(archive)
			z := zip.NewWriter(f)
			header := &zip.FileHeader{Name: "release/rclone", Method: zip.Deflate}
			header.SetMode(0o700)
			if mode == "missing" {
				header.Name = "../../escape"
			}
			if mode == "symlink" {
				header.SetMode(os.ModeSymlink | 0o700)
			}
			w, _ := z.CreateHeader(header)
			_, _ = w.Write([]byte("binary"))
			if mode == "duplicate" {
				duplicate := *header
				w, _ = z.CreateHeader(&duplicate)
				_, _ = w.Write([]byte("different"))
			}
			_ = z.Close()
			_ = f.Close()
			dest := filepath.Join(dir, "rclone")
			err := extractBinary(archive, "release/rclone", dest)
			if (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
			if mode != "valid" {
				if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid member installed")
				}
			}
		})
	}
}

func TestOfficialRcloneDownload(t *testing.T) {
	if os.Getenv("UNARR_TEST_DEPENDENCY_DOWNLOAD") != "1" {
		t.Skip("opt-in official dependency download")
	}
	t.Setenv("PATH", t.TempDir())
	opts := Options{Directory: t.TempDir(), Output: os.Stdout}
	path, err := ensureRclone(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ensureRclone(context.Background(), opts)
	if err != nil || again != path {
		t.Fatal(again, err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("downloaded rclone again instead of reusing it")
	}
}

func TestLinuxPackageSelection(t *testing.T) {
	for _, manager := range []string{"apt-get", "dnf", "yum", "pacman", "zypper", "apk"} {
		command, err := linuxPackageCommand(func(name string) (string, error) {
			if name == manager {
				return "/bin/" + name, nil
			}
			return "", os.ErrNotExist
		})
		if err != nil || command[0] != "/bin/"+manager || command[len(command)-1] != "fuse3" {
			t.Fatal(command, err)
		}
	}
}
