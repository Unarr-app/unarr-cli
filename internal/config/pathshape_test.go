package config

import (
	"strings"
	"testing"
)

func TestPathShapeError(t *testing.T) {
	cases := []struct {
		name    string
		dir     string
		goos    string
		wantErr bool
		why     string
	}{
		{
			name:    "the crash report: a drive letter repeated mid-path",
			dir:     `D:\D:\`,
			goos:    "windows",
			wantErr: true,
			why:     "this shape failed every daemon start twelve times and was reported as a permission problem",
		},
		{
			name:    "a colon deeper in the path is just as impossible",
			dir:     `D:\Media\C:\downloads`,
			goos:    "windows",
			wantErr: true,
			why:     "Windows forbids ':' outside the volume name at any depth",
		},
		{name: "a normal Windows path", dir: `D:\.unarr`, goos: "windows", why: "the working config from the same agent"},
		{name: "drive root", dir: `D:\`, goos: "windows", why: "shape is fine; dangerousPaths is what rejects a drive root"},
		{name: "bare drive", dir: `D:`, goos: "windows", why: "drive-relative, but nothing malformed about it"},
		{name: "relative path", dir: `Media\unarr`, goos: "windows", why: "no volume name, no colon"},
		{
			name: "UNC share", dir: `\\nas\media\unarr`, goos: "windows",
			why: "UNC is not parsed for drive letters at all",
		},
		{
			name: "extended-length prefix keeps its own colon", dir: `\\?\C:\Media`, goos: "windows",
			why: "rejecting this would break users who typed a perfectly valid path",
		},
		{
			name: "a colon on linux is a legal filename byte", dir: `/media/D:/downloads`, goos: "linux",
			why: "the rule is Windows-only on purpose — POSIX allows ':' in names",
		},
		{name: "linux path", dir: "/home/user/Media", goos: "linux", why: "ordinary"},
		{name: "darwin path with a colon", dir: "/Users/u/My:Folder", goos: "darwin", why: "legal on APFS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pathShapeError("downloads.dir", tc.dir, tc.goos)
			if tc.wantErr && err == nil {
				t.Fatalf("pathShapeError(%q, %s) = nil, want an error — %s", tc.dir, tc.goos, tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("pathShapeError(%q, %s) = %v, want nil — %s", tc.dir, tc.goos, err, tc.why)
			}
		})
	}
}

// TestPathShapeErrorNamesTheKey: the message is what the user acts on, so it
// must say WHICH setting is wrong and what it currently holds.
func TestPathShapeErrorNamesTheKey(t *testing.T) {
	err := pathShapeError("downloads.dir", `D:\D:\`, "windows")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"downloads.dir", `D:\D:\`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err.Error(), want)
		}
	}
}
