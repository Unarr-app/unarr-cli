package mediainfo

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestInstallBinaryAtomically: concurrent installers of the same tool must
// never leave a partial file at dest, and the winner's bytes must be complete.
func TestInstallBinaryAtomically(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin", "tool")
	payload := []byte(strings.Repeat("x", 1<<20))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := installBinaryAtomically(dest, payload); err != nil {
				t.Errorf("install: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := os.ReadFile(dest)
	if err != nil || len(got) != len(payload) {
		t.Fatalf("dest = %d bytes, err %v; want %d", len(got), err, len(payload))
	}
	st, _ := os.Stat(dest)
	if st.Mode()&0o111 == 0 {
		t.Fatalf("dest is not executable: %v", st.Mode())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), "*.tmp-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}
