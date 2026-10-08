package yenc

import (
	"strings"
	"testing"
)

func TestBoundaryYEncIntegrity(t *testing.T) {
	for name, body := range map[string]string{
		"no trailer":              "=ybegin line=128 size=1 name=v.mkv\nk\n",
		"trailer length":          "=ybegin line=128 size=1 name=v.mkv\nk\n=yend size=2\n",
		"header length":           "=ybegin line=128 size=2 name=v.mkv\nk\n=yend size=1\n",
		"zero checksum present":   "=ybegin line=128 size=1 name=v.mkv\nk\n=yend size=1 crc32=00000000\n",
		"malformed checksum":      "=ybegin line=128 size=1 name=v.mkv\nk\n=yend size=1 crc32=zzzzzzzz\n",
		"multipart missing range": "=ybegin part=1 total=2 line=128 size=2 name=v.mkv\nk\n=yend size=1 part=1\n",
		"multipart bounds":        "=ybegin part=1 total=2 line=128 size=2 name=v.mkv\n=ypart begin=2 end=3\nk\n=yend size=1 part=1\n",
		"multipart number":        "=ybegin part=1 total=2 line=128 size=2 name=v.mkv\n=ypart begin=1 end=1\nk\n=yend size=1 part=2\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(body)); err == nil {
				t.Error("Decode accepted incomplete/corrupt article")
			}
			if _, err := DecodeInPlace([]byte(body)); err == nil {
				t.Error("DecodeInPlace accepted incomplete/corrupt article")
			}
		})
	}
}
