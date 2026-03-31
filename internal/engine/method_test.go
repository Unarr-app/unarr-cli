package engine

import "testing"

func TestDownloadMethodConstants(t *testing.T) {
	if MethodTorrent != "torrent" {
		t.Errorf("MethodTorrent = %q, want torrent", MethodTorrent)
	}
	if MethodDebrid != "debrid" {
		t.Errorf("MethodDebrid = %q, want debrid", MethodDebrid)
	}
	if MethodUsenet != "usenet" {
		t.Errorf("MethodUsenet = %q, want usenet", MethodUsenet)
	}
}

func TestProgressStruct(t *testing.T) {
	p := Progress{
		DownloadedBytes: 1024,
		TotalBytes:      2048,
		SpeedBps:        512,
		ETA:             10,
		Peers:           5,
		Seeds:           3,
		FileName:        "movie.mkv",
	}

	if p.DownloadedBytes != 1024 {
		t.Errorf("DownloadedBytes = %d, want 1024", p.DownloadedBytes)
	}
	if p.FileName != "movie.mkv" {
		t.Errorf("FileName = %q, want movie.mkv", p.FileName)
	}
}

func TestResultStruct(t *testing.T) {
	r := Result{
		FilePath: "/downloads/movie.mkv",
		FileName: "movie.mkv",
		Method:   MethodTorrent,
		Size:     1073741824,
	}

	if r.Method != MethodTorrent {
		t.Errorf("Method = %q, want torrent", r.Method)
	}
	if r.Size != 1073741824 {
		t.Errorf("Size = %d, want 1073741824", r.Size)
	}
}
