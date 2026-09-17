package library

import (
	"os"
	"path/filepath"
	"testing"
)

// The bug these tests pin (La Brea S01E01, 2026-09-17): a torrent client
// preallocates every file of a pack at its final size, so a release that never
// downloaded a byte is a 553 MB apparent / 0 block sparse file. discoverFiles
// gated on info.Size() — the APPARENT size — so the stub cleared the 100 MB
// floor, was scanned, and got published as a playable library_item. The web
// offered it in the player and handed VLC a URL; both died with "Invalid data
// found when processing input", with nothing in the UI pointing at the cause.

// writeSparse creates a file with `size` apparent bytes and (on a filesystem
// that supports holes) zero allocated blocks — what a preallocating torrent
// client leaves behind for a file it has not started.
func writeSparse(t *testing.T, path string, size int64) os.FileInfo {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestIsZeroContentStubFlagsSparseFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "La Brea - Cap.101.avi")
	info := writeSparse(t, path, minFileSize*2)

	if diskUsage(info) != 0 {
		// ZFS/NTFS/tmpfs variants may materialise the truncate. The zero-header
		// signal still covers the file, which the next assertion proves.
		t.Logf("filesystem allocated %d bytes for the hole — sparse signal not exercised", diskUsage(info))
	}

	stub, why := isZeroContentStub(path, info)
	if !stub {
		t.Fatalf("a %d-byte apparent file with no content must be a stub, got (%v, %q)", info.Size(), stub, why)
	}
	if why == "" {
		t.Error("a stub must carry a reason — it is logged so the skip is not silent")
	}
}

func TestIsZeroContentStubFlagsZeroedHeader(t *testing.T) {
	// The non-sparse variant: the preallocation was materialised as real zero
	// blocks (common on the NFS/SMB NAS mounts agents use) and on Windows, where
	// diskUsage falls back to the apparent size and the sparse signal is a no-op.
	root := t.TempDir()
	path := filepath.Join(root, "zerohead.mkv")
	writeHeadMidTail(t, path, 3*fpChunk, 0x00, 0xAB, 0xCD)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if diskUsage(info) == 0 {
		t.Fatal("file was written with real bytes — the sparse signal must NOT be what flags it")
	}

	stub, _ := isZeroContentStub(path, info)
	if !stub {
		t.Error("a file whose header is all NUL has no container magic and must be skipped")
	}
}

func TestHeaderAllZeroIsBoundedToOnePage(t *testing.T) {
	// The probe window is headerProbe (4 KiB), NOT fpChunk: this check runs in
	// discoverFiles, ahead of the incremental cache, so it is charged to every
	// file on every cycle — 1 MiB per file was ~10 GB of reads per scan on a
	// 10k-file NAS library.
	//
	// Two consequences, both pinned here so neither is an accident:
	//   - a file that is NUL for 4 KiB and real afterwards is still skipped. No
	//     container this scanner indexes pads its magic behind 4 KiB of zeros
	//     (RIFF/ftyp/EBML sit at offset 0, MPEG-TS's sync inside 188 bytes), so
	//     there is nothing legitimate in that window to lose.
	//   - a file with real magic at offset 0 is kept even if the rest of the
	//     first MiB is a hole, which is what a just-started download looks like.
	root := t.TempDir()

	zeroPage := filepath.Join(root, "zeropage.mkv")
	writeHeadMidTail(t, zeroPage, 3*fpChunk, 0x00, 0xAB, 0xCD)
	if zero, err := HeaderAllZero(zeroPage, 3*fpChunk); err != nil || !zero {
		t.Errorf("NUL header must be flagged, got (%v, %v)", zero, err)
	}

	// Real magic in the first page, holes behind it.
	started := filepath.Join(root, "started.mkv")
	f, err := os.Create(started)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("RIFF....AVI ")); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(3 * fpChunk); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if zero, err := HeaderAllZero(started, 3*fpChunk); err != nil || zero {
		t.Errorf("magic at offset 0 must be kept however sparse the rest is, got (%v, %v)", zero, err)
	}
}

func TestIsZeroContentStubKeepsPartialDownloadWithRealHeader(t *testing.T) {
	// Deliberately NOT a completeness check. A download at ~86 % with a real
	// header plays from the start, so it must stay indexed exactly as before —
	// there is no percentage threshold in the gate to regress.
	root := t.TempDir()
	path := filepath.Join(root, "partial.mkv")
	writeHeadMidTail(t, path, 3*fpChunk, 0x1A, 0x45, 0x00) // real header, ZEROED TAIL

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if stub, why := isZeroContentStub(path, info); stub {
		t.Errorf("a partially-written file with a real header must be kept, skipped as %q", why)
	}
}

func TestIsZeroContentStubKeepsRealFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "real.mkv")
	writeHeadMidTail(t, path, 3*fpChunk, 0x1A, 0x45, 0xDF)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if stub, why := isZeroContentStub(path, info); stub {
		t.Errorf("a file with real content must never be a stub, got %q", why)
	}
}

func TestIsZeroContentStubKeepsUnreadableFile(t *testing.T) {
	// "Can't read it" is not "it's empty": an EACCES or a torn NFS handle must
	// not silently drop a real file out of the user's library.
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}
	root := t.TempDir()
	path := filepath.Join(root, "locked.mkv")
	writeHeadMidTail(t, path, 3*fpChunk, 0x1A, 0x45, 0xDF)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if stub, why := isZeroContentStub(path, info); stub {
		t.Errorf("an unreadable file must be kept, skipped as %q", why)
	}
}

func TestDiscoverFilesSkipsSparseStubButKeepsSibling(t *testing.T) {
	// End-to-end over the walker, in the exact shape the incident had: one pack
	// directory, one file never started and one with real content.
	root := t.TempDir()
	stub := filepath.Join(root, "La Brea - Cap.101.avi")
	real := filepath.Join(root, "La Brea - Cap.102.avi")

	writeSparse(t, stub, minFileSize*2)
	writeHeadMidTail(t, real, minFileSize+fpChunk, 0x52, 0x49, 0x46)

	files, err := discoverFiles(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		if f == stub {
			t.Error("the sparse stub reached the scan — this is the La Brea bug")
		}
	}
	found := false
	for _, f := range files {
		if f == real {
			found = true
		}
	}
	if !found {
		t.Errorf("the real sibling must still be discovered, got %v", files)
	}
}
