package engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRedactURLMasksXtreamCredentials(t *testing.T) {
	raw := "http://panel.example:8080/series/alice/s%2Fcr3t/501.mkv"
	base := errors.New(`Get "` + raw + `": dial tcp: connection refused (via /series/alice/s/cr3t/)`)
	got := redactURL(base, raw)
	msg := got.Error()
	if strings.Contains(msg, "alice") || strings.Contains(msg, "cr3t") {
		t.Fatalf("credentials survived: %s", msg)
	}
	if !strings.Contains(msg, "panel.example:8080") || !strings.Contains(msg, "501.mkv") {
		t.Fatalf("redaction removed more than the credentials: %s", msg)
	}
	if !errors.Is(got, base) {
		t.Fatal("the original error must stay reachable for classification")
	}
}

func TestRedactURLShortCredentialsOnlyMaskedAsAPair(t *testing.T) {
	raw := "http://p.example/movie/u/pw/1.mkv"
	got := redactURL(errors.New(`Get "`+raw+`": unexpected EOF`), raw).Error()
	if strings.Contains(got, "/u/pw/") {
		t.Fatalf("credential pair survived: %s", got)
	}
	if !strings.Contains(got, "unexpected EOF") {
		t.Fatalf("a short credential mangled the rest of the message: %s", got)
	}
}

func TestRedactURLLeavesUnrelatedErrorsAlone(t *testing.T) {
	base := errors.New("disk full")
	if got := redactURL(base, "http://p/movie/u/p/1.mkv"); got != base {
		t.Fatalf("an error without the URL must be returned as is, got %v", got)
	}
	if redactURL(nil, "http://p/movie/u/p/1.mkv") != nil {
		t.Fatal("nil stays nil")
	}
}

func TestIptvDownloadErrorNeverCarriesTheCredentials(t *testing.T) {
	// A closed port: the transfer fails with a *url.Error naming the full URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	d := NewIptvDownloader(NewPlaybackHold(time.Minute))
	task := iptvTask("leak", "http://"+addr+"/movie/bob/hunter2/7.mkv")
	_, derr := d.Download(context.Background(), task, t.TempDir(), make(chan Progress, 100))
	if derr == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(derr.Error(), "hunter2") || strings.Contains(derr.Error(), "bob") {
		t.Fatalf("error leaks the Xtream credentials: %v", derr)
	}
}
