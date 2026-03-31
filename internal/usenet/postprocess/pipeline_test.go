package postprocess

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindPar2File(t *testing.T) {
	dir := t.TempDir()

	// Create par2 files of different sizes
	mainPar2 := filepath.Join(dir, "content.par2")
	vol1 := filepath.Join(dir, "content.vol000+01.par2")
	vol2 := filepath.Join(dir, "content.vol001+02.par2")

	os.WriteFile(mainPar2, make([]byte, 100), 0o644)   // smallest
	os.WriteFile(vol1, make([]byte, 10000), 0o644)
	os.WriteFile(vol2, make([]byte, 50000), 0o644)

	files := map[string]string{
		"content.par2":            mainPar2,
		"content.vol000+01.par2": vol1,
		"content.vol001+02.par2": vol2,
	}

	result := findPar2File(files)
	if result != mainPar2 {
		t.Errorf("findPar2File() = %q, want %q (smallest par2)", result, mainPar2)
	}
}

func TestFindPar2FileNone(t *testing.T) {
	files := map[string]string{
		"video.mkv": "/tmp/video.mkv",
		"subs.srt":  "/tmp/subs.srt",
	}

	result := findPar2File(files)
	if result != "" {
		t.Errorf("findPar2File() = %q, want empty", result)
	}
}

func TestFindPar2FileEmpty(t *testing.T) {
	result := findPar2File(map[string]string{})
	if result != "" {
		t.Errorf("findPar2File() = %q, want empty", result)
	}
}

func TestFindFirstRarPart01(t *testing.T) {
	files := map[string]string{
		"movie.part01.rar": "/tmp/movie.part01.rar",
		"movie.part02.rar": "/tmp/movie.part02.rar",
		"movie.part03.rar": "/tmp/movie.part03.rar",
	}

	result := findFirstRar(files)
	if result != "/tmp/movie.part01.rar" {
		t.Errorf("findFirstRar() = %q, want part01.rar", result)
	}
}

func TestFindFirstRarSingle(t *testing.T) {
	files := map[string]string{
		"movie.rar": "/tmp/movie.rar",
		"movie.r00": "/tmp/movie.r00",
		"movie.r01": "/tmp/movie.r01",
	}

	result := findFirstRar(files)
	if result != "/tmp/movie.rar" {
		t.Errorf("findFirstRar() = %q, want movie.rar (shortest)", result)
	}
}

func TestFindFirstRarSplitFormat(t *testing.T) {
	files := map[string]string{
		"movie.001": "/tmp/movie.001",
		"movie.002": "/tmp/movie.002",
	}

	result := findFirstRar(files)
	if result != "/tmp/movie.001" {
		t.Errorf("findFirstRar() = %q, want movie.001", result)
	}
}

func TestFindFirstRarNone(t *testing.T) {
	files := map[string]string{
		"video.mkv": "/tmp/video.mkv",
		"subs.srt":  "/tmp/subs.srt",
	}

	result := findFirstRar(files)
	if result != "" {
		t.Errorf("findFirstRar() = %q, want empty", result)
	}
}

func TestFindMainFile(t *testing.T) {
	dir := t.TempDir()

	// Create video files of different sizes
	small := filepath.Join(dir, "small.mkv")
	large := filepath.Join(dir, "large.mkv")
	nonVideo := filepath.Join(dir, "readme.txt")

	os.WriteFile(small, make([]byte, 1000), 0o644)
	os.WriteFile(large, make([]byte, 5000), 0o644)
	os.WriteFile(nonVideo, make([]byte, 9000), 0o644)

	result := findMainFile(dir, []string{small, large, nonVideo})
	if result != large {
		t.Errorf("findMainFile() = %q, want %q (largest video)", result, large)
	}
}

func TestFindMainFileFallbackToDir(t *testing.T) {
	dir := t.TempDir()

	video := filepath.Join(dir, "movie.mp4")
	os.WriteFile(video, make([]byte, 5000), 0o644)

	// Pass empty file list — should fallback to scanning dir
	result := findMainFile(dir, nil)
	if result != video {
		t.Errorf("findMainFile() = %q, want %q (dir scan fallback)", result, video)
	}
}

func TestFindMainFileEmpty(t *testing.T) {
	dir := t.TempDir()
	result := findMainFile(dir, nil)
	if result != "" {
		t.Errorf("findMainFile() = %q, want empty", result)
	}
}

func TestFindMainFileMultipleFormats(t *testing.T) {
	dir := t.TempDir()

	mkv := filepath.Join(dir, "movie.mkv")
	mp4 := filepath.Join(dir, "movie.mp4")
	avi := filepath.Join(dir, "movie.avi")

	os.WriteFile(mkv, make([]byte, 3000), 0o644)
	os.WriteFile(mp4, make([]byte, 5000), 0o644) // largest
	os.WriteFile(avi, make([]byte, 2000), 0o644)

	result := findMainFile(dir, []string{mkv, mp4, avi})
	if result != mp4 {
		t.Errorf("findMainFile() = %q, want %q", result, mp4)
	}
}
