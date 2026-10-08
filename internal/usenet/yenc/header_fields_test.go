package yenc

import (
	"fmt"
	"testing"
)

func TestFinalHeaderFieldsExcludeFilename(t *testing.T) {
	for _, name := range []string{"video.mkv", "part=1.mkv", "show.total=2.mkv", "size=999.mkv", "a part=1 total=2 size=99.mkv", "name=part=1.mkv"} {
		for _, size := range []string{"size=1 ", "size=0 ", ""} {
			body := "=ybegin line=128 " + size + "name=" + name + "\r\nk\r\n=yend size=1 crc32=d3d99e8b\r\n"
			for _, decoder := range []struct {
				name string
				call func([]byte) (*Part, error)
			}{{"Decode", DecodeBytes}, {"DecodeInPlace", DecodeInPlace}} {
				t.Run(fmt.Sprintf("%s/%s/%s", decoder.name, size, name), func(t *testing.T) {
					part, err := decoder.call([]byte(body))
					if err != nil {
						t.Fatal("valid filename interpreted as header fields", err)
					}
					if part.Name != name || part.Number != 0 || part.Total != 0 || part.Size != 1 || part.Begin != 1 || part.End != 1 || string(part.Data) != "A" {
						t.Fatalf("filename altered part metadata: %+v", part)
					}
				})
			}
		}
	}
}

func TestFinalExactMultipartHeaderFields(t *testing.T) {
	body := []byte("=ybegin xpart=9 part=1 xtotal=9 total=2 xsize=9 size=2 line=128 name=part=2 total=9.mkv\r\n=ypart begin=1 end=1\r\nk\r\n")
	var decoder InPlaceDecoder
	decoder.Feed(body)
	start, end, decoded, ok := decoder.Progress()
	if !ok || start != 0 || end != 1 || decoded != 1 || body[0] != 'A' {
		t.Fatalf("partial multipart progress changed: %d %d %d %v", start, end, decoded, ok)
	}
	body = append(body, []byte("=yend size=1 part=1 pcrc32=d3d99e8b\r\n")...)
	part, err := decoder.Finish(body)
	if err != nil || part.Number != 1 || part.Total != 2 || part.Size != 2 || string(part.Data) != "A" {
		t.Fatalf("exact multipart header fields: %+v, %v", part, err)
	}
	if _, err := DecodeBytes([]byte("=ybegin part=bad total=2 size=2 line=128 name=part=1.mkv\r\n=ypart begin=1 end=1\r\nk\r\n=yend size=1 part=1\r\n")); err == nil {
		t.Fatal("malformed actual part field accepted")
	}
}
