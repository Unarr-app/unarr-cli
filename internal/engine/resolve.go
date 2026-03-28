package engine

import (
	"context"
	"fmt"
	"log"
)

// resolveMethod determines which download method to use for a task.
// For "auto": tries available methods in priority order (torrent > debrid > usenet).
// For specific method: uses only that method.
func resolveMethod(ctx context.Context, task *Task, downloaders map[DownloadMethod]Downloader) (DownloadMethod, error) {
	var order []DownloadMethod
	switch task.PreferredMethod {
	case "torrent":
		order = []DownloadMethod{MethodTorrent}
	case "debrid":
		order = []DownloadMethod{MethodDebrid}
	case "usenet":
		order = []DownloadMethod{MethodUsenet}
	default: // "auto"
		order = []DownloadMethod{MethodTorrent, MethodDebrid, MethodUsenet}
	}

	for _, method := range order {
		// Skip already-tried methods
		tried := false
		for _, tm := range task.TriedMethods {
			if tm == method {
				tried = true
				break
			}
		}
		if tried {
			continue
		}

		dl, ok := downloaders[method]
		if !ok {
			continue // downloader not registered
		}

		available, err := dl.Available(ctx, task)
		if err != nil {
			taskID := task.ID
			if len(taskID) > 8 {
				taskID = taskID[:8]
			}
			log.Printf("[%s] %s availability check failed: %v", taskID, method, err)
			continue
		}
		if available {
			return method, nil
		}
	}

	return "", fmt.Errorf("no download method available (tried: %v)", task.TriedMethods)
}

// tryFallback attempts to fall back to the next untried download method.
// Returns true if fallback was initiated, false if no more methods.
func tryFallback(task *Task, downloaders map[DownloadMethod]Downloader) bool {
	if task.PreferredMethod != "auto" {
		return false // specific method requested, no fallback
	}

	task.TriedMethods = append(task.TriedMethods, task.ResolvedMethod)

	available := make([]DownloadMethod, 0, len(downloaders))
	for m := range downloaders {
		available = append(available, m)
	}

	return task.HasUntried(available)
}
