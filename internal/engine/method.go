package engine

import "context"

// DownloadMethod identifies a download strategy.
type DownloadMethod string

const (
	MethodTorrent DownloadMethod = "torrent"
	MethodDebrid  DownloadMethod = "debrid"
	MethodUsenet  DownloadMethod = "usenet"
)

// Progress is emitted by downloaders during a download.
type Progress struct {
	DownloadedBytes int64
	TotalBytes      int64
	SpeedBps        int64 // bytes per second
	ETA             int   // seconds remaining
	Peers           int   // connected peers (torrent only)
	Seeds           int   // connected seeds (torrent only)
	FileName        string
}

// Result is returned when a download completes successfully.
type Result struct {
	FilePath string
	FileName string
	Method   DownloadMethod
	Size     int64
}

// Downloader is the interface every download method must implement.
type Downloader interface {
	// Method returns which method this downloader implements.
	Method() DownloadMethod

	// Available reports whether this method can handle the given task.
	// For torrent: always true if infoHash is set.
	// For debrid: checks if cached on debrid service.
	// For usenet: checks if NZB is available.
	Available(ctx context.Context, task *Task) (bool, error)

	// Download starts the download. It blocks until completion or error.
	// Progress is reported via progressCh at regular intervals.
	// outputDir is where files should be written.
	Download(ctx context.Context, task *Task, outputDir string, progressCh chan<- Progress) (*Result, error)

	// Pause suspends an in-progress download but keeps partial files on disk
	// so the download can be resumed later.
	Pause(taskID string) error

	// Cancel aborts an in-progress download and removes partial files.
	Cancel(taskID string) error

	// Shutdown gracefully shuts down the downloader.
	Shutdown(ctx context.Context) error
}
