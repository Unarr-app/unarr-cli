package cmd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
)

// handleMountTask receives an NZB selected on the website and places only its
// manifest in the inbox watched by `unarr mount`. The media remains remote and
// is read from NNTP on demand by the mounted filesystem.
func handleMountTask(ctx context.Context, task agent.Task, cfg config.Config, client *agent.Client) {
	if _, err := client.ReportStatus(ctx, agent.StatusUpdate{
		TaskID: task.ID, Status: "resolving", ResolvedMethod: "usenet",
	}); err != nil {
		log.Printf("[%s] mount task start report failed: %v", agent.ShortID(task.ID), err)
	}

	path, err := addNZBToMountInbox(ctx, task, cfg, client)
	reportCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	update := agent.StatusUpdate{TaskID: task.ID, ResolvedMethod: "usenet"}
	if err != nil {
		update.Status = "failed"
		update.ErrorMessage = err.Error()
		log.Printf("[%s] could not add NZB to mount: %v", agent.ShortID(task.ID), err)
	} else {
		update.Status = "completed"
		update.Progress = 100
		update.FileName = filepath.Base(path)
		log.Printf("[%s] NZB added to mount inbox: %s", agent.ShortID(task.ID), path)
	}
	if _, reportErr := client.ReportStatus(reportCtx, update); reportErr != nil {
		log.Printf("[%s] mount task final report failed: %v", agent.ShortID(task.ID), reportErr)
	}
}

func addNZBToMountInbox(
	ctx context.Context,
	task agent.Task,
	cfg config.Config,
	client *agent.Client,
) (string, error) {
	if task.NzbID == "" {
		return "", fmt.Errorf("mount task has no NZB")
	}
	data, err := client.DownloadNzb(ctx, task.NzbID)
	if err != nil {
		return "", fmt.Errorf("download NZB manifest: %w", err)
	}
	dir := cfg.Mount.NZBDirectory(resolvedConfigPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create mount NZB inbox: %w", err)
	}
	path := filepath.Join(dir, mountNZBFileName(task.Title, task.NzbID))
	if err := writeMountManifest(path, data); err != nil {
		return "", err
	}
	return path, nil
}

func mountNZBFileName(title, id string) string {
	base := strings.TrimSpace(title)
	if base == "" {
		base = "Usenet release"
	}
	base = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, base)
	base = strings.Trim(base, ". ")
	runes := []rune(base)
	if len(runes) > 120 {
		base = string(runes[:120])
	}
	suffix := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))[:12]
	return fmt.Sprintf("%s [%s].nzb", base, suffix)
}

func writeMountManifest(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nzb-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary NZB: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary NZB: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write NZB manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync NZB manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close NZB manifest: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// Windows cannot atomically replace an existing file. The target is the
		// exact stable path for this NZB, never a user-controlled directory.
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("replace NZB manifest: %w", err)
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("install NZB manifest: %w", err)
		}
	}
	return nil
}
