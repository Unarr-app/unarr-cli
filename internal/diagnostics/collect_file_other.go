//go:build !linux && !darwin && !freebsd

package diagnostics

import "os"

func openDiagnosticFile(path string) (*os.File, error) { return os.Open(path) }
