package nntptest

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
)

// This file exposes the NON-streamable and RAR5 fixtures the streaming
// classifier's tests need: a compressed archive and an encrypted archive (both
// must be REJECTED and fall back to the batch download) plus a RAR5 STORE archive
// (must be accepted). Each mirrors BuildRarStore's contract: it returns an NZB
// and a message-id -> yEnc body map ready for FakeServer.AddArticles.

const rar4CompressedMethod = 0x33 // a normal (non-store) RAR4 compression method

// BuildRarCompressed builds a multi-volume RAR4 archive whose file uses a
// compression method (not store). The classifier must reject it: its bytes are
// not verbatim, so it cannot be streamed.
func BuildRarCompressed(videoName string, content []byte, volSize, partSize int) (*nzb.NZB, map[string][]byte) {
	volumes := buildRar4(videoName, content, volSize, rar4CompressedMethod, false)
	return postRarVolumes(videoName, volumes, partSize, classicVolumeName)
}

// BuildRarEncrypted builds a multi-volume RAR4 STORE archive whose file header
// has the password (encryption) flag set. The classifier must reject it.
func BuildRarEncrypted(videoName string, content []byte, volSize, partSize int) (*nzb.NZB, map[string][]byte) {
	volumes := buildRar4(videoName, content, volSize, rarStoreMethod, true)
	return postRarVolumes(videoName, volumes, partSize, classicVolumeName)
}

// BuildRarStore5 builds a multi-volume RAR5 STORE archive (new-style
// ".partNN.rar" naming). Like BuildRarStore, the video bytes appear verbatim
// inside the concatenated volumes, so the classifier must accept it and stream it.
func BuildRarStore5(videoName string, content []byte, volSize, partSize int) (*nzb.NZB, map[string][]byte) {
	volumes := buildRar5Store(videoName, content, volSize)
	return postRarVolumes(videoName, volumes, partSize, partVolumeName)
}

// postRarVolumes turns raw RAR volume containers into an NZB + article map: each
// volume becomes one NZB File (named via nameFn) split into yEnc articles.
func postRarVolumes(videoName string, volumes [][]byte, partSize int, nameFn func(base string, i int) string) (*nzb.NZB, map[string][]byte) {
	archiveBase := strings.TrimSuffix(videoName, filepath.Ext(videoName))
	if archiveBase == "" {
		archiveBase = "archive"
	}
	articles := make(map[string][]byte)
	files := make([]nzb.File, 0, len(volumes))
	for i, vol := range volumes {
		volName := nameFn(archiveBase, i)
		segs, arts := postFile(volName, vol, partSize)
		for id, body := range arts {
			articles[id] = body
		}
		files = append(files, newFile(volName, len(segs), segs))
	}
	return &nzb.NZB{Files: files, Meta: map[string]string{}}, articles
}

// classicVolumeName is the ".rar/.r00/.r01" naming used by RAR4 fixtures.
func classicVolumeName(base string, i int) string { return rarVolumeName(base, i) }

// partVolumeName is the new-style ".part01.rar/.part02.rar" naming used by RAR5.
func partVolumeName(base string, i int) string {
	return fmt.Sprintf("%s.part%02d.rar", base, i+1)
}
