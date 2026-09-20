package mountsetup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/fsx"
)

const maxDownload = 150 << 20
const maxBinary = 200 << 20

// Download to a private temporary file and verify BEFORE opening an archive or
// executing an installer. A cancelled/partial/oversized download never installs.
func download(ctx context.Context, client *http.Client, a artifact, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dependency download: HTTP %d", resp.StatusCode)
	}
	f, err := os.CreateTemp(dir, "download-*")
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return "", err
	}
	if n > maxDownload {
		return "", fmt.Errorf("dependency download exceeds size limit")
	}
	if hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return "", fmt.Errorf("dependency checksum mismatch; nothing installed")
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	keep = true
	return f.Name(), nil
}

func extractBinary(archive, member, dest string) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer z.Close()
	var found *zip.File
	for _, f := range z.File {
		if f.Name != member {
			continue
		}
		if found != nil || !f.Mode().IsRegular() || f.UncompressedSize64 > maxBinary {
			return fmt.Errorf("invalid dependency archive member")
		}
		found = f
	}
	if found == nil {
		return fmt.Errorf("dependency archive is missing %s", member)
	}
	r, err := found.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	return installBinary(r, dest)
}

func installBinary(r io.Reader, dest string) error {
	f, err := os.CreateTemp(filepath.Dir(dest), "rclone-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	n, err := io.Copy(f, io.LimitReader(r, maxBinary+1))
	if err != nil {
		return err
	}
	if n == 0 || n > maxBinary {
		return fmt.Errorf("invalid rclone binary size")
	}
	if err := f.Chmod(0o700); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return fsx.RenameWithRetry(f.Name(), dest, 2*time.Second, 50*time.Millisecond)
}
