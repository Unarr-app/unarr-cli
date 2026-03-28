package engine

import (
	"context"
	"fmt"
)

// UsenetDownloader downloads via Usenet/NZB protocol.
// Currently a stub — not implemented.
type UsenetDownloader struct{}

func NewUsenetDownloader() *UsenetDownloader { return &UsenetDownloader{} }

func (u *UsenetDownloader) Method() DownloadMethod { return MethodUsenet }

func (u *UsenetDownloader) Available(_ context.Context, _ *Task) (bool, error) {
	return false, nil // always unavailable until implemented
}

func (u *UsenetDownloader) Download(_ context.Context, _ *Task, _ string, _ chan<- Progress) (*Result, error) {
	return nil, fmt.Errorf("usenet download not implemented yet (coming in a future release)")
}

func (u *UsenetDownloader) Pause(_ string) error             { return nil }
func (u *UsenetDownloader) Cancel(_ string) error            { return nil }
func (u *UsenetDownloader) Shutdown(_ context.Context) error { return nil }
