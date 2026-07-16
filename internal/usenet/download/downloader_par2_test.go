package download

import (
	"context"
	"testing"
)

// TestDownloadPar2Files_EmptyInput guards the input contract: an empty par2
// list is an error, not a silent empty map — the engine relies on this to know
// whether it fetched anything (the map feeds the Corrupt classification).
func TestDownloadPar2Files_EmptyInput(t *testing.T) {
	_, err := (&Downloader{}).DownloadPar2Files(context.Background(), nil, t.TempDir(), nil)
	if err == nil {
		t.Fatal("DownloadPar2Files(nil) = nil error, want error for empty par2 list")
	}
}
