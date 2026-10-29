package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSetDownloadDirCarriesOrganizeFoldersInsideTheOldDir(t *testing.T) {
	old, nu, elsewhere := filepath.FromSlash("/c/Videos"), filepath.FromSlash("/d/Downloads"), filepath.FromSlash("/nas/Films")
	var c Config
	c.Download.Dir = old
	c.Organize.MoviesDir = filepath.Join(old, "Movies")
	c.Organize.TVShowsDir = elsewhere

	c.SetDownloadDir(nu)

	if c.Download.Dir != nu {
		t.Errorf("download dir = %q, want %q", c.Download.Dir, nu)
	}
	if want := filepath.Join(nu, "Movies"); c.Organize.MoviesDir != want {
		t.Errorf("movies dir = %q, want %q (it lived inside the old download dir)", c.Organize.MoviesDir, want)
	}
	if c.Organize.TVShowsDir != elsewhere {
		t.Errorf("tv dir = %q, want it untouched: the user chose it outside the download dir", c.Organize.TVShowsDir)
	}
}

func TestSetDownloadDirKeepsTheOldDirScannedOnlyWhenScanPathIsFree(t *testing.T) {
	var c Config
	c.Download.Dir = "/old"
	c.SetDownloadDir("/new")
	if c.Library.ScanPath != "/old" {
		t.Errorf("scan path = %q, want the old dir so existing files stay in the library", c.Library.ScanPath)
	}
	c.Library.ScanPath = "/user/choice"
	c.SetDownloadDir("/newer")
	if c.Library.ScanPath != "/user/choice" {
		t.Errorf("scan path = %q, a user-set scan path must not be overwritten", c.Library.ScanPath)
	}
}

func TestRebaseUnderExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got := rebaseUnder(filepath.Join(home, "Media", "Movies"), "~/Media", filepath.FromSlash("/d/x"))
	if want := filepath.Join(filepath.FromSlash("/d/x"), "Movies"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDriveRootErrorOnlyForWholeWindowsDrives(t *testing.T) {
	for _, p := range []string{`E:\`, `e:/`, `Z:`, ` F:\ `} {
		if DriveRootError("downloads.dir", p, "windows") == nil {
			t.Errorf("%q accepted on windows, want rejection", p)
		}
	}
	for _, p := range []string{`E:\Downloads`, `\\nas\share`, `relative`, ""} {
		if err := DriveRootError("downloads.dir", p, "windows"); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	if DriveRootError("downloads.dir", `E:\`, "linux") != nil {
		t.Error("drive-root rule must only apply on windows")
	}
	if err := DriveRootError("downloads.dir", `e:/`, "windows"); err == nil || !strings.Contains(err.Error(), `e:\Downloads`) {
		t.Errorf("message should suggest a folder on the drive, got %v", err)
	}
}

func TestRebaseUnderLeavesLookalikesAlone(t *testing.T) {
	root := filepath.FromSlash("/c/Videos")
	for _, p := range []string{
		"",
		filepath.FromSlash("/c/VideosOld"), // prefix match is not containment
		filepath.FromSlash("/c/Other/Movies"),
	} {
		if got := rebaseUnder(p, root, filepath.FromSlash("/d/x")); got != p {
			t.Errorf("rebaseUnder(%q) = %q, want unchanged", p, got)
		}
	}
}

func TestRebaseUnderMovesAFolderEqualToTheOldDir(t *testing.T) {
	// movies_dir = downloads.dir: finished movies must follow to the new drive.
	root, nu := filepath.FromSlash("/c/Media"), filepath.FromSlash("/e/Media")
	if got := rebaseUnder(root, root, nu); got != nu {
		t.Errorf("rebaseUnder(root) = %q, want %q", got, nu)
	}
}

func TestSetDownloadDirClearingIsNotAMove(t *testing.T) {
	var c Config
	c.Download.Dir = filepath.FromSlash("/data/dl")
	c.Organize.MoviesDir = filepath.FromSlash("/data/dl/Movies")
	c.SetDownloadDir("")
	if c.Organize.MoviesDir != filepath.FromSlash("/data/dl/Movies") || c.Library.ScanPath != "" {
		t.Errorf("clearing moved things: movies=%q scan=%q", c.Organize.MoviesDir, c.Library.ScanPath)
	}
}
