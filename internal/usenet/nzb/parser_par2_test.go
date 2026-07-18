package nzb

import "testing"

// nzbWithSubjects builds an NZB whose files have the given quoted-filename
// subjects and per-file sizes (one segment each).
func nzbWithSubjects(subjects []string, sizes []int64) *NZB {
	n := &NZB{}
	for i, s := range subjects {
		n.Files = append(n.Files, File{
			Subject:  `"` + s + `" yEnc (1/1)`,
			Segments: []Segment{{Bytes: sizes[i], Number: 1, MessageID: "id"}},
		})
	}
	return n
}

func TestPar2IndexFile_PrefersNonVolume(t *testing.T) {
	n := nzbWithSubjects(
		[]string{"movie.mkv", "release.vol000+01.par2", "release.par2", "release.vol001+02.par2"},
		[]int64{5000, 900, 1200, 1800},
	)
	idx := n.Par2IndexFile()
	if idx == nil || idx.Filename() != "release.par2" {
		t.Fatalf("Par2IndexFile() = %v, want release.par2 (non-volume preferred even when a volume is smaller)", idx)
	}
	vols := n.Par2VolumeFiles()
	if len(vols) != 2 {
		t.Fatalf("Par2VolumeFiles() len = %d, want 2", len(vols))
	}
	for _, v := range vols {
		if v.Filename() == "release.par2" || v.Filename() == "movie.mkv" {
			t.Errorf("unexpected volume %q", v.Filename())
		}
	}
}

func TestPar2IndexFile_FallsBackToSmallestVolume(t *testing.T) {
	n := nzbWithSubjects(
		[]string{"movie.mkv", "release.vol000+01.par2", "release.vol001+02.par2"},
		[]int64{5000, 300, 900},
	)
	idx := n.Par2IndexFile()
	if idx == nil || idx.Filename() != "release.vol000+01.par2" {
		t.Fatalf("Par2IndexFile() = %v, want smallest volume", idx)
	}
	if vols := n.Par2VolumeFiles(); len(vols) != 1 || vols[0].Filename() != "release.vol001+02.par2" {
		t.Fatalf("Par2VolumeFiles() = %v, want only the remaining volume", vols)
	}
}

func TestPar2IndexFile_NoParity(t *testing.T) {
	n := nzbWithSubjects([]string{"movie.mkv", "movie.nfo"}, []int64{5000, 10})
	if idx := n.Par2IndexFile(); idx != nil {
		t.Fatalf("Par2IndexFile() = %v, want nil", idx)
	}
	if vols := n.Par2VolumeFiles(); len(vols) != 0 {
		t.Fatalf("Par2VolumeFiles() = %v, want empty", vols)
	}
}

func TestIsObfuscatedName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"a1b2c3d4e5f60718.mkv", true},
		{"ABCDEF0123456789abcdef.rar", true},
		{"Movie.2024.1080p.BluRay.x264-GRP.mkv", false},
		{"short.mkv", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsObfuscatedName(c.name); got != c.want {
			t.Errorf("IsObfuscatedName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
