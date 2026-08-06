//go:build kfbench

package mediainfo

// Manual measurement harness for the keyframe index over a real NFS mount.
// Not part of the normal suite (build tag `kfbench`) because it needs a real
// multi-GB media file and takes minutes on a cold cache.
//
//	go test ./internal/library/mediainfo/ -tags kfbench -run TestKeyframeBench \
//	  -timeout 30m -v -args -kfbench.media="/path/to/file.mkv"

import (
	"context"
	"flag"
	"testing"
	"time"
)

var benchMedia = flag.String("kfbench.media", "", "media file to index")

func TestKeyframeBench(t *testing.T) {
	if *benchMedia == "" {
		t.Skip("set -args -kfbench.media=<path>")
	}
	ffprobe, err := ResolveFFprobe("")
	if err != nil {
		t.Skipf("ffprobe unavailable: %v", err)
	}

	t0 := time.Now()
	kfs, ok := ReadCachedKeyframes(*benchMedia)
	t.Logf("CACHE-READ ok=%v n=%d elapsed=%v", ok, len(kfs), time.Since(t0))

	t1 := time.Now()
	got, err := IndexKeyframes(context.Background(), ffprobe, *benchMedia)
	elapsed := time.Since(t1)
	if err != nil {
		t.Logf("INDEX FAILED after %v: %v", elapsed, err)
		return
	}
	t.Logf("INDEX ok n=%d elapsed=%v", len(got), elapsed)
}
