package nntptest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// sampleVideo returns deterministic pseudo-random bytes standing in for a video
// file. A simple LCG keeps it reproducible without importing math/rand.
func sampleVideo(n int) []byte {
	b := make([]byte, n)
	x := uint32(0x1234_5678)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 16)
	}
	return b
}

// reassembleFile fetches every article of an nzb.File through the client, in
// part-number order, decodes the yEnc parts and concatenates them — the same
// reconstruction the download path performs.
func reassembleFile(t *testing.T, c *nntp.Client, f nzb.File) []byte {
	t.Helper()
	segs := append([]nzb.Segment(nil), f.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].Number < segs[j].Number })
	var out []byte
	for _, seg := range segs {
		raw, err := c.Body(context.Background(), seg.MessageID)
		if err != nil {
			t.Fatalf("Body %s: %v", seg.MessageID, err)
		}
		part, err := yenc.DecodeBytes(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", seg.MessageID, err)
		}
		out = append(out, part.Data...)
	}
	return out
}

func TestBuildDirectFileReassembles(t *testing.T) {
	content := sampleVideo(10_000)
	n, articles := BuildDirectFile("movie.2024.1080p.mkv", content, 1500)

	if got := n.TotalSegments(); got != 7 { // ceil(10000/1500)
		t.Errorf("segments = %d, want 7", got)
	}
	if len(n.ContentFiles()) != 1 {
		t.Errorf("ContentFiles = %d, want 1", len(n.ContentFiles()))
	}
	if n.Files[0].Filename() != "movie.2024.1080p.mkv" {
		t.Errorf("Filename = %q", n.Files[0].Filename())
	}

	s := NewFakeServer(t)
	s.AddArticles(articles)
	c := dialClient(t, s)

	got := reassembleFile(t, c, n.Files[0])
	if !bytes.Equal(got, content) {
		t.Fatalf("reassembled direct file mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

func TestBuildRarStoreEmbedsVerbatimMultiVolume(t *testing.T) {
	content := sampleVideo(20_000)
	const volSize = 7000
	n, articles := BuildRarStore("show.s01e01.mkv", content, volSize, 1200)

	if !n.HasRars() {
		t.Fatal("HasRars() = false, want true")
	}
	if len(n.RarFiles()) != 3 { // ceil(20000/7000) volumes
		t.Errorf("RarFiles = %d, want 3", len(n.RarFiles()))
	}
	if n.Files[0].Filename() != "show.s01e01.rar" {
		t.Errorf("first volume = %q, want show.s01e01.rar", n.Files[0].Filename())
	}

	s := NewFakeServer(t)
	s.AddArticles(articles)
	c := dialClient(t, s)

	// STORE invariant, hermetically: each volume contains its content chunk
	// verbatim (method 0 = no compression), and volume 0 opens with the marker.
	for i, f := range n.Files {
		vol := reassembleFile(t, c, f)
		if i == 0 && !bytes.HasPrefix(vol, rar4Marker) {
			t.Error("first volume does not start with RAR4 marker")
		}
		start := i * volSize
		end := min(start+volSize, len(content))
		if !bytes.Contains(vol, content[start:end]) {
			t.Fatalf("volume %d (%s) does not embed its stored chunk verbatim", i, f.Filename())
		}
	}
}

func TestBuildRarStoreSingleVolumeContiguous(t *testing.T) {
	content := sampleVideo(5_000)
	// volSize larger than the file → one volume; the stored bytes are then one
	// contiguous verbatim run inside the container.
	n, articles := BuildRarStore("clip.mkv", content, 1<<20, 900)

	if len(n.Files) != 1 {
		t.Fatalf("files = %d, want 1 volume", len(n.Files))
	}
	s := NewFakeServer(t)
	s.AddArticles(articles)
	c := dialClient(t, s)

	vol := reassembleFile(t, c, n.Files[0])
	if bytes.Index(vol, content) < 0 {
		t.Fatal("single-volume container does not embed content contiguously")
	}
}

// TestBuildRarStoreUnrarReadable cross-validates the hand-built RAR against a
// real extractor (unrar or 7z). It is skipped when neither is on PATH so the
// suite stays hermetic, but on dev/CI machines that have them it proves the
// headers are spec-correct, not merely store-embedded.
func TestBuildRarStoreUnrarReadable(t *testing.T) {
	tool, args := findRarExtractor()
	if tool == "" {
		t.Skip("no unrar/7z on PATH; skipping real-extractor cross-check")
	}

	content := sampleVideo(18_500)
	n, articles := BuildRarStore("clip.mkv", content, 6000, 1000)

	s := NewFakeServer(t)
	s.AddArticles(articles)
	c := dialClient(t, s)

	dir := t.TempDir()
	for _, f := range n.Files {
		vol := reassembleFile(t, c, f)
		if err := os.WriteFile(filepath.Join(dir, f.Filename()), vol, 0o600); err != nil {
			t.Fatalf("write volume %s: %v", f.Filename(), err)
		}
	}

	cmd := exec.Command(tool, append(args, filepath.Join(dir, "clip.rar"))...) //nolint:gosec // fixed test tool
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s failed: %v", tool, err)
	}
	if !bytes.Equal(out, content) {
		t.Fatalf("%s extracted %d bytes, want %d (headers likely wrong)", tool, len(out), len(content))
	}
}

// findRarExtractor returns a tool and the args that print the archive's file to
// stdout, or ("", nil) if none is available.
func findRarExtractor() (string, []string) {
	if p, err := exec.LookPath("unrar"); err == nil {
		return p, []string{"p", "-inul", "-p-"} // print file, no messages, no password prompt
	}
	if p, err := exec.LookPath("7z"); err == nil {
		return p, []string{"e", "-so"} // extract to stdout
	}
	return "", nil
}
