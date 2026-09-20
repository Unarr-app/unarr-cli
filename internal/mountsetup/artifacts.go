package mountsetup

import "fmt"

// Official release SHA256SUMS / GitHub release asset digests, checked 2026-09-20.
// Pin updates deliberately; never execute a moving "latest" download.
const rcloneVersion = "1.75.1"

type artifact struct {
	URL, SHA256, Member string
}

var rcloneHashes = map[string]string{
	"linux-amd64":   "982b5aa772841168f8e380f139e9e787b2a105403e32b94da8676a0e1c0a13ab",
	"linux-arm64":   "03f2504174034b6d004152ed7369251c9a9ec1f7e0836eda420f5c7a5ec0dff9",
	"osx-amd64":     "29253d0288b8fbbac46baad6e5f6add6cb01d462c79f10805bbd4631c4cdf82c",
	"osx-arm64":     "c61d7a371c62bcbbe882c3423aa4b8bf63485c248dd0f692997b8f0c3f6d0c6f",
	"windows-amd64": "200eb602c126d82aa38b51e0f6b9ae837473ff99b51278d3f6f837574c494d6e",
	"windows-arm64": "c3c6cd0424dd49076ad179c30c3f9e5cde2c004ec07ea9fe6911f23e32eafe0f",
}

func rcloneArtifact(goos, arch string) (artifact, error) {
	if goos == "darwin" {
		goos = "osx"
	}
	key := goos + "-" + arch
	hash, ok := rcloneHashes[key]
	if !ok {
		return artifact{}, fmt.Errorf("automatic rclone setup is unavailable for %s", key)
	}
	stem := "rclone-v" + rcloneVersion + "-" + key
	name := "rclone"
	if goos == "windows" {
		name += ".exe"
	}
	return artifact{URL: "https://downloads.rclone.org/v" + rcloneVersion + "/" + stem + ".zip", SHA256: hash, Member: stem + "/" + name}, nil
}

var winfspArtifact = artifact{
	URL:    "https://github.com/winfsp/winfsp/releases/download/v2.1/winfsp-2.1.25156.msi",
	SHA256: "073a70e00f77423e34bed98b86e600def93393ba5822204fac57a29324db9f7a",
}
var macfuseArtifact = artifact{
	URL:    "https://github.com/macfuse/macfuse/releases/download/macfuse-5.4.0/macfuse-5.4.0.dmg",
	SHA256: "861814f0ac7fa8f6547ea40cdd49a36ac84bcc7d34f38a1fa74e8cf68b0401c5",
}
