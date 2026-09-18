package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestReportRequiresSeparateCompleteConsents(t *testing.T) {
	for _, tt := range []struct {
		input                  string
		collected, sent, saved int
	}{
		{"", 0, 0, 0}, {"yes", 0, 0, 0}, {"no\n", 0, 0, 0},
		{"yes\ncancel\n", 1, 0, 0}, {"yes\nsend\n", 1, 0, 0},
		{"yes\nsend\nyes", 1, 0, 0}, {"yes\nsend\nno\n", 1, 0, 0},
		{"yes\nsave\n", 1, 0, 1}, {"yes\nsend\nyes\n", 1, 1, 0},
		{strings.Repeat("y", 5000) + "\n", 0, 0, 0},
	} {
		t.Run(tt.input, func(t *testing.T) {
			var collected, sent, saved int
			var out bytes.Buffer
			payload := []byte(`{"schemaVersion":1}`)
			actions := reportActions{
				collect: func() ([]byte, error) { collected++; return payload, nil },
				send: func(_ context.Context, b []byte) error {
					sent++
					if !bytes.Equal(b, payload) || !strings.Contains(out.String(), string(b)) {
						t.Fatal("sent unreviewed bytes")
					}
					return nil
				},
				save: func(b []byte) (string, error) {
					saved++
					if !bytes.Equal(b, payload) {
						t.Fatal("saved different bytes")
					}
					return "report.json", nil
				},
			}
			if err := runReport(context.Background(), strings.NewReader(tt.input), &out, actions); err != nil {
				t.Fatal(err)
			}
			if collected != tt.collected || sent != tt.sent || saved != tt.saved {
				t.Fatalf("got collect/send/save %d/%d/%d", collected, sent, saved)
			}
		})
	}
}

func TestReportFailureRetainsReviewedBytesWithoutRawError(t *testing.T) {
	var out bytes.Buffer
	var saved bool
	err := runReport(context.Background(), strings.NewReader("yes\nsend\nyes\n"), &out, reportActions{
		collect: func() ([]byte, error) { return []byte(`{}`), nil },
		send:    func(context.Context, []byte) error { return errors.New("secret-token alice@example.test 192.168.1.1") },
		save:    func(b []byte) (string, error) { saved = bytes.Equal(b, []byte(`{}`)); return "report.json", nil },
	})
	if !errors.Is(err, errQuietExit) || !saved {
		t.Fatalf("err=%v saved=%v", err, saved)
	}
	if strings.Contains(out.String(), "secret-token") {
		t.Fatal("raw transport error leaked")
	}
}

func TestReportSavePrivateAndExclusive(t *testing.T) {
	t.Chdir(t.TempDir())
	a, err := savePrivateReport([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := savePrivateReport([]byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("overwrote existing report")
	}
	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatal("report accessible to other users")
	}
}

func TestPrivateReportInvocation(t *testing.T) {
	for _, args := range [][]string{{"report"}, {"--config", "private", "reports"}, {"report", "--invalid"}} {
		if !PrivateReportRequested(args) {
			t.Fatal(args)
		}
	}
	cmd := newReportCmd()
	if cmd.PersistentPreRun == nil {
		t.Fatal("inherited authenticated pre-run")
	}
}
