//go:build !windows

package cmd

import "os"

func createPrivateReportFile() (*os.File, error) {
	return os.CreateTemp(".", "unarr-report-*.json") // O_EXCL, mode 0600.
}
