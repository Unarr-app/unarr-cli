package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSrc creates a source file with given content under tmp/<sub>/<name>.
func writeSrc(t *testing.T, tmp, sub, name, content string) string {
	t.Helper()
	dir := filepath.Join(tmp, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A 4K grab landing after a 1080p one must coexist, not overwrite it.
func TestOrganizeSecondVersionCoexists(t *testing.T) {
	tmp := t.TempDir()
	moviesDir := filepath.Join(tmp, "Movies")
	year := 2023
	cfg := OrganizeConfig{Enabled: true, MoviesDir: moviesDir}

	src1 := writeSrc(t, tmp, "src1", "Oppenheimer.2023.1080p.mkv", "v1080")
	r1 := &Result{FilePath: src1, FileName: "Oppenheimer.2023.1080p.mkv", Method: MethodTorrent}
	t1 := &Task{ContentType: "movie", ContentTitle: "Oppenheimer", ContentYear: &year, Title: "Oppenheimer 2023 1080p BluRay x264"}
	p1, err := organize(r1, t1, cfg)
	if err != nil {
		t.Fatal(err)
	}

	src2 := writeSrc(t, tmp, "src2", "Oppenheimer.2023.2160p.HDR.mkv", "v2160")
	r2 := &Result{FilePath: src2, FileName: "Oppenheimer.2023.2160p.HDR.mkv", Method: MethodTorrent}
	t2 := &Task{ContentType: "movie", ContentTitle: "Oppenheimer", ContentYear: &year, Title: "Oppenheimer 2023 2160p HDR BluRay x265"}
	p2, err := organize(r2, t2, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if p1 == p2 {
		t.Fatalf("second version overwrote the first (both at %s)", p1)
	}
	if got := filepath.Base(p1); got != "Oppenheimer (2023).mkv" {
		t.Errorf("first name = %q, want clean deterministic name", got)
	}
	if got := filepath.Base(p2); got != "Oppenheimer (2023) [2160p HDR].mkv" {
		t.Errorf("second name = %q, want version-tagged name", got)
	}
	if b, _ := os.ReadFile(p1); string(b) != "v1080" {
		t.Errorf("first file clobbered: content = %q", b)
	}
	if b, _ := os.ReadFile(p2); string(b) != "v2160" {
		t.Errorf("second file content = %q, want v2160", b)
	}
}

// Same resolution from a different source (torrent then usenet) must also coexist.
func TestOrganizeSameQualityDifferentSourceCoexists(t *testing.T) {
	tmp := t.TempDir()
	moviesDir := filepath.Join(tmp, "Movies")
	year := 2023
	cfg := OrganizeConfig{Enabled: true, MoviesDir: moviesDir}

	src1 := writeSrc(t, tmp, "src1", "Dune.2021.1080p.mkv", "torrent")
	r1 := &Result{FilePath: src1, FileName: "Dune.2021.1080p.mkv", Method: MethodTorrent}
	t1 := &Task{ContentType: "movie", ContentTitle: "Dune", ContentYear: &year, Title: "Dune 2021 1080p WEB x264"}
	p1, err := organize(r1, t1, cfg)
	if err != nil {
		t.Fatal(err)
	}

	src2 := writeSrc(t, tmp, "src2", "Dune.2021.1080p.usenet.mkv", "usenet")
	r2 := &Result{FilePath: src2, FileName: "Dune.2021.1080p.usenet.mkv", Method: MethodUsenet}
	t2 := &Task{ContentType: "movie", ContentTitle: "Dune", ContentYear: &year, Title: "Dune 2021 1080p WEB x264"}
	p2, err := organize(r2, t2, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if p1 == p2 {
		t.Fatalf("same-quality second source overwrote the first (both at %s)", p1)
	}
	if b, _ := os.ReadFile(p1); string(b) != "torrent" {
		t.Errorf("first file clobbered: content = %q", b)
	}
	if b, _ := os.ReadFile(p2); string(b) != "usenet" {
		t.Errorf("second file content = %q, want usenet", b)
	}
}

// organize NEVER clobbers at the move stage, even for an upgrade task: the new
// file lands on a free sibling so finalizeVerified's replaceFile can back the
// old file up BEFORE swapping (renaming straight onto ReplacePath would destroy
// it first). The swap itself is exercised at the manager level, not here.
func TestOrganizeUpgradeCoexistsAtOrganizeStage(t *testing.T) {
	tmp := t.TempDir()
	moviesDir := filepath.Join(tmp, "Movies")
	year := 2020
	cfg := OrganizeConfig{Enabled: true, MoviesDir: moviesDir}

	src1 := writeSrc(t, tmp, "src1", "Tenet.2020.1080p.mkv", "old")
	r1 := &Result{FilePath: src1, FileName: "Tenet.2020.1080p.mkv", Method: MethodTorrent}
	t1 := &Task{ContentType: "movie", ContentTitle: "Tenet", ContentYear: &year, Title: "Tenet 2020 1080p"}
	p1, err := organize(r1, t1, cfg)
	if err != nil {
		t.Fatal(err)
	}

	src2 := writeSrc(t, tmp, "src2", "Tenet.2020.2160p.mkv", "new")
	r2 := &Result{FilePath: src2, FileName: "Tenet.2020.2160p.mkv", Method: MethodTorrent}
	t2 := &Task{ContentType: "movie", ContentTitle: "Tenet", ContentYear: &year, Title: "Tenet 2020 2160p", ReplacePath: p1}
	p2, err := organize(r2, t2, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if p2 == p1 {
		t.Fatalf("upgrade clobbered the incumbent at the organize stage (%s)", p1)
	}
	if b, _ := os.ReadFile(p1); string(b) != "old" {
		t.Errorf("organize destroyed the incumbent: content = %q, want old", b)
	}
	if b, _ := os.ReadFile(p2); string(b) != "new" {
		t.Errorf("new file content = %q, want new", b)
	}
}

// A directory result (multi-file torrent) must also coexist — the RemoveAll that
// used to clobber it is gone.
func TestOrganizeDirResultCoexists(t *testing.T) {
	tmp := t.TempDir()
	moviesDir := filepath.Join(tmp, "Movies")
	year := 2019
	cfg := OrganizeConfig{Enabled: true, MoviesDir: moviesDir}

	// Dot-free dir names so the derived extension stays empty and both grabs map
	// to the same deterministic folder/name (forcing the collision path).
	mkDirResult := func(sub, content string) *Result {
		d := filepath.Join(tmp, sub)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "payload.bin"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return &Result{FilePath: d, FileName: sub, Method: MethodTorrent}
	}

	t1 := &Task{ContentType: "movie", ContentTitle: "Joker", ContentYear: &year, Title: "Joker 2019 1080p"}
	p1, err := organize(mkDirResult("jokerA", "v1"), t1, cfg)
	if err != nil {
		t.Fatal(err)
	}

	t2 := &Task{ContentType: "movie", ContentTitle: "Joker", ContentYear: &year, Title: "Joker 2019 2160p"}
	p2, err := organize(mkDirResult("jokerB", "v2"), t2, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if p1 == p2 {
		t.Fatalf("second directory clobbered the first (both at %s)", p1)
	}
	for _, p := range []string{p1, p2} {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			t.Errorf("expected a directory at %s (err=%v)", p, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(p1, "payload.bin")); string(b) != "v1" {
		t.Errorf("first directory clobbered: content = %q", b)
	}
}

// A sidecar subtitle must follow the video to its FINAL (version-tagged) name so
// the pair stays matched after a coexist redirect.
func TestOrganizeSubtitleFollowsRenamedVersion(t *testing.T) {
	tmp := t.TempDir()
	moviesDir := filepath.Join(tmp, "Movies")
	year := 2021
	cfg := OrganizeConfig{Enabled: true, MoviesDir: moviesDir}

	// First version takes the deterministic name.
	src1 := writeSrc(t, tmp, "s1", "Dune.mkv", "v1")
	if _, err := organize(
		&Result{FilePath: src1, FileName: "Dune.mkv", Method: MethodTorrent},
		&Task{ContentType: "movie", ContentTitle: "Dune", ContentYear: &year, Title: "Dune 2021 1080p"},
		cfg,
	); err != nil {
		t.Fatal(err)
	}

	// Second version + a sidecar subtitle → coexists as a tagged sibling.
	src2 := writeSrc(t, tmp, "s2", "Dune.2160p.mkv", "v2")
	if err := os.WriteFile(filepath.Join(tmp, "s2", "Dune.2160p.en.srt"), []byte("subs"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := organize(
		&Result{FilePath: src2, FileName: "Dune.2160p.mkv", Method: MethodTorrent},
		&Task{ContentType: "movie", ContentTitle: "Dune", ContentYear: &year, Title: "Dune 2021 2160p HDR"},
		cfg,
	)
	if err != nil {
		t.Fatal(err)
	}

	base := strings.TrimSuffix(filepath.Base(p2), filepath.Ext(p2)) // "Dune (2021) [2160p HDR]"
	wantSub := filepath.Join(filepath.Dir(p2), base+".en.srt")
	if b, err := os.ReadFile(wantSub); err != nil || string(b) != "subs" {
		t.Errorf("subtitle did not follow the renamed version to %s (err=%v)", wantSub, err)
	}
}

func TestVersionTag(t *testing.T) {
	cases := []struct{ title, want string }{
		{"Oppenheimer 2023 2160p HDR BluRay x265", "2160p HDR"},
		{"Oppenheimer 2023 1080p Castellano", "1080p ES"},
		{"Oppenheimer 2023 1080p BluRay", "1080p"},
		{"Some.Movie.4K.DV.2020", "2160p DV"},
		{"Movie 2019 1080p HDR10", "1080p HDR10"},
		{"Movie 720p Latino", "720p LAT"},
		{"Movie English 1080p", "1080p EN"},
		{"Nada reconocible aqui", ""},
	}
	for _, c := range cases {
		if got := versionTag(&Task{Title: c.title}); got != c.want {
			t.Errorf("versionTag(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

// The collision chain: base → [tag] → [tag method] → counter, never occupied.
func TestVersionDistinctPathChain(t *testing.T) {
	dir := t.TempDir()
	occupy := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &Result{Method: MethodUsenet}
	task := &Task{Title: "Movie 2023 1080p"}

	occupy("Movie (2023).mkv")
	if got := filepath.Base(versionDistinctPath(dir, "Movie (2023).mkv", r, task)); got != "Movie (2023) [1080p].mkv" {
		t.Fatalf("step 1 = %q", got)
	}
	occupy("Movie (2023) [1080p].mkv")
	if got := filepath.Base(versionDistinctPath(dir, "Movie (2023).mkv", r, task)); got != "Movie (2023) [1080p usenet].mkv" {
		t.Fatalf("step 2 = %q", got)
	}
	occupy("Movie (2023) [1080p usenet].mkv")
	if got := filepath.Base(versionDistinctPath(dir, "Movie (2023).mkv", r, task)); got != "Movie (2023) [1080p] (2).mkv" {
		t.Fatalf("step 3 = %q", got)
	}
}
