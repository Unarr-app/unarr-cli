package engine

import (
	"context"
	"fmt"

	tc "github.com/torrentclaw/go-client"
)

// DebridDownloader downloads via debrid services (Real-Debrid, AllDebrid, etc.).
// Currently a stub — Available() works, Download() returns not-implemented.
type DebridDownloader struct {
	apiClient *tc.Client
}

// NewDebridDownloader creates a debrid downloader stub.
func NewDebridDownloader(apiClient *tc.Client) *DebridDownloader {
	return &DebridDownloader{apiClient: apiClient}
}

func (d *DebridDownloader) Method() DownloadMethod { return MethodDebrid }

func (d *DebridDownloader) Available(ctx context.Context, task *Task) (bool, error) {
	if d.apiClient == nil {
		return false, nil
	}
	resp, err := d.apiClient.DebridCheckCache(ctx, "", "", []string{task.InfoHash})
	if err != nil {
		return false, err
	}
	cached, ok := resp.Cached[task.InfoHash]
	return ok && cached, nil
}

func (d *DebridDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	return nil, fmt.Errorf("debrid download not implemented yet (coming in a future release)")
}

func (d *DebridDownloader) Pause(_ string) error            { return nil }
func (d *DebridDownloader) Cancel(_ string) error            { return nil }
func (d *DebridDownloader) Shutdown(_ context.Context) error { return nil }
