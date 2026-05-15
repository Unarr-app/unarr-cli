// Package engine — validate.go centralises input validators used by the
// stream/HLS HTTP handlers and the daemon glue. Keep new validators in this
// file so a future reviewer can audit the trust boundary in one place.
package engine

import "regexp"

// validSessionID restricts session IDs to characters safe for use as a single
// filesystem path component. Server-issued UUIDs and hex strings match this;
// anything containing slashes, dots, or path separators is rejected so a
// compromised or buggy server cannot escape hlsTmpDirRoot via os.MkdirAll.
var validSessionID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
