package engine

import (
	"context"
	"fmt"
	"log"
)

// effectiveOrder returns the ordered methods to try for a task.
//
// The agent's local config (configMethods, from config.toml `preferred_methods`)
// WINS and gates: only the listed methods are eligible, in that order — so a
// "debrid only" agent never tries torrent even if the web's task says otherwise.
// When the config has no explicit preference (nil), we fall back to the per-task
// preference the web sent: a specific method runs alone; "auto" tries all three
// torrent-first (the historical default).
func effectiveOrder(task *Task, configMethods []string) []DownloadMethod {
	if len(configMethods) > 0 {
		order := make([]DownloadMethod, 0, len(configMethods))
		for _, m := range configMethods {
			switch m {
			case "torrent":
				order = append(order, MethodTorrent)
			case "debrid":
				order = append(order, MethodDebrid)
			case "usenet":
				order = append(order, MethodUsenet)
			}
		}
		if len(order) > 0 {
			return order
		}
	}
	switch task.PreferredMethod {
	case "torrent":
		return []DownloadMethod{MethodTorrent}
	case "debrid":
		return []DownloadMethod{MethodDebrid}
	case "usenet":
		return []DownloadMethod{MethodUsenet}
	default: // "auto"
		return []DownloadMethod{MethodTorrent, MethodDebrid, MethodUsenet}
	}
}

// resolveMethod determines which download method to use for a task, honouring the
// agent's configured method order (gating) over the per-task preference.
func resolveMethod(ctx context.Context, task *Task, downloaders map[DownloadMethod]Downloader, configMethods []string) (DownloadMethod, error) {
	order := effectiveOrder(task, configMethods)

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

	return "", fmt.Errorf("no download method available (order: %v, tried: %v)", order, task.TriedMethods)
}

// tryFallback attempts to fall back to the next untried download method WITHIN
// the effective order. A single-method order (e.g. "debrid only") has no
// fallback — failing over to torrent would defeat the whole preference.
func tryFallback(task *Task, downloaders map[DownloadMethod]Downloader, configMethods []string) bool {
	order := effectiveOrder(task, configMethods)
	if len(order) <= 1 {
		return false // single method requested, no fallback
	}

	task.TriedMethods = append(task.TriedMethods, task.ResolvedMethod)

	for _, m := range order {
		tried := false
		for _, tm := range task.TriedMethods {
			if tm == m {
				tried = true
				break
			}
		}
		if tried {
			continue
		}
		if _, ok := downloaders[m]; ok {
			return true
		}
	}
	return false
}
