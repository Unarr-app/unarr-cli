package mountsetup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Historical official SHA256SUMS. These are test inputs, never offered to users.
var historicalHashes = map[string]map[string]string{
	"1.53.4": {
		"linux-amd64":   "5fd9abd29b8aa1ab0f195c33f78a476a6221d651b55ad4f913e3559b9904abc6",
		"linux-arm64":   "68e0887977f3aabc5449bb66033e8a00811abe09a7e95354d1ff48a34965c594",
		"osx-amd64":     "5ed167e073d2c620120ecdf392458146d6c3db480e4097a600dfcf0fb3db2c3b",
		"windows-amd64": "91651b5200cd8e7145dfe4aba227bfd03be356b6cbcb5c973f446fb0186c3776",
	},
	"1.68.2": {
		"linux-amd64":   "0e6fa18051e67fc600d803a2dcb10ddedb092247fc6eee61be97f64ec080a13c",
		"linux-arm64":   "c6e9d4cf9c88b279f6ad80cd5675daebc068e404890fa7e191412c1bc7a4ac5f",
		"osx-amd64":     "cdc685e16abbf35b6f47c95b2a5b4ad73a73921ff6842e5f4136c8b461756188",
		"osx-arm64":     "323f387b32bcf9ddfc3874f01879a0b2689dbd91309beb8c3a4410db04d0c41f",
		"windows-amd64": "812bf76cc02c04cf6327f3683f3d5a88e47d36c39db84c1a745777496be7d993",
		"windows-arm64": "cbc6584266cf62bb9f4df912cb00d566c1cbc50ce2748f5e433f1937209e807e",
	},
}

func TestOfficialHistoricalRclone(t *testing.T) {
	if os.Getenv("UNARR_TEST_DEPENDENCY_DOWNLOAD") != "1" {
		t.Skip("opt-in official download")
	}
	for _, version := range []string{"1.53.4", "1.68.2"} {
		t.Run(version, func(t *testing.T) {
			path := historicalBinary(t, version)
			t.Setenv("PATH", filepath.Dir(path))
			wantReuse := version == "1.68.2"
			if got := mountCapable(context.Background(), path); got != wantReuse {
				t.Fatalf("v%s capability=%v, want %v", version, got, wantReuse)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := ensureRclone(context.Background(), Options{Directory: t.TempDir(), Output: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			if (resolved == path) != wantReuse {
				t.Fatal("incorrect reuse/replacement", resolved)
			}
			after, err := os.Stat(path)
			if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("modified user's old binary")
			}
			t.Logf("%s/%s rclone %s: reused=%v; original preserved", runtime.GOOS, runtime.GOARCH, version, wantReuse)
		})
	}
}

func historicalBinary(t *testing.T, version string) string {
	t.Helper()
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "osx"
	}
	key := platform + "-" + runtime.GOARCH
	hash := historicalHashes[version][key]
	if hash == "" {
		t.Skipf("official v%s has no native %s artifact", version, key)
	}
	stem := fmt.Sprintf("rclone-v%s-%s", version, key)
	name := "rclone"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	a := artifact{URL: fmt.Sprintf("https://downloads.rclone.org/v%s/%s.zip", version, stem), SHA256: hash, Member: stem + "/" + name}
	dir := t.TempDir()
	archive, err := download(context.Background(), &http.Client{Timeout: 3 * time.Minute}, a, dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := extractBinary(archive, a.Member, path); err != nil {
		t.Fatal(err)
	}
	return path
}
