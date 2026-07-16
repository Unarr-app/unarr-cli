package nntptest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// BuildDirectFile builds a synthetic NZB and its yEnc articles for a single
// video file posted directly (no RAR), split into ceil(len/partSize) parts.
// It returns the NZB (one File) and a message-id -> raw yEnc body map ready for
// FakeServer.AddArticles. The returned articles round-trip through yenc.Decode
// and reassemble byte-for-byte into content.
func BuildDirectFile(name string, content []byte, partSize int) (*nzb.NZB, map[string][]byte) {
	segs, articles := postFile(name, content, partSize)
	n := &nzb.NZB{
		Files: []nzb.File{newFile(name, len(segs), segs)},
		Meta:  map[string]string{},
	}
	return n, articles
}

// BuildRarStore builds a synthetic NZB and yEnc articles for a video posted
// inside a multi-volume RAR archive using the STORE method (0% compression).
// videoName is the file inside the archive (e.g. "movie.mkv"); volSize caps the
// bytes per RAR volume and partSize caps the bytes per yEnc article within a
// volume. Each RAR volume becomes one NZB File (".rar", ".r00", …), split into
// articles. Because the method is store, videoName's bytes appear verbatim in
// the concatenated volumes — the invariant the RAR-store stream reader uses.
func BuildRarStore(videoName string, content []byte, volSize, partSize int) (*nzb.NZB, map[string][]byte) {
	archiveBase := strings.TrimSuffix(videoName, filepath.Ext(videoName))
	if archiveBase == "" {
		archiveBase = "archive"
	}
	volumes := buildRar4Store(videoName, content, volSize)

	articles := make(map[string][]byte)
	files := make([]nzb.File, 0, len(volumes))
	for i, vol := range volumes {
		volName := rarVolumeName(archiveBase, i)
		segs, arts := postFile(volName, vol, partSize)
		for id, body := range arts {
			articles[id] = body
		}
		files = append(files, newFile(volName, len(segs), segs))
	}
	n := &nzb.NZB{Files: files, Meta: map[string]string{}}
	return n, articles
}

// postFile chops content into yEnc articles and returns the matching NZB
// segments (ordered by part number) plus the message-id -> body map. It is the
// shared engine behind both the direct-file and RAR-store builders.
func postFile(name string, content []byte, partSize int) ([]nzb.Segment, map[string][]byte) {
	if partSize <= 0 {
		partSize = len(content)
	}
	if partSize <= 0 {
		partSize = 1
	}
	total := (len(content) + partSize - 1) / partSize
	if total == 0 {
		total = 1
	}

	articles := make(map[string][]byte, total)
	segs := make([]nzb.Segment, 0, total)
	base := sanitizeID(name)
	fileSize := int64(len(content))

	for i := 0; i < total; i++ {
		start := i * partSize
		end := start + partSize
		if end > len(content) {
			end = len(content)
		}
		partNum := i + 1
		body := yenc.Encode(name, partNum, total, int64(start)+1, int64(end), fileSize, content[start:end])
		msgID := fmt.Sprintf("%s-p%d@fake.local", base, partNum)
		articles[msgID] = body
		segs = append(segs, nzb.Segment{
			Bytes:     int64(len(body)),
			Number:    partNum,
			MessageID: msgID,
		})
	}
	return segs, articles
}

// newFile builds an nzb.File whose subject carries the quoted filename that
// nzb.File.Filename() extracts.
func newFile(name string, total int, segs []nzb.Segment) nzb.File {
	return nzb.File{
		Subject:  fmt.Sprintf(`[nntptest] "%s" yEnc (1/%d)`, name, total),
		Groups:   []string{"alt.binaries.test"},
		Segments: segs,
	}
}

var nonIDChars = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// sanitizeID derives a stable, unique-per-name message-id stem from a filename.
func sanitizeID(name string) string {
	id := nonIDChars.ReplaceAllString(name, "-")
	id = strings.Trim(id, "-")
	if id == "" {
		id = "file"
	}
	return id
}
