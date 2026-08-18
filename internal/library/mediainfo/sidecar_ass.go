package mediainfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

// Raw .ass extraction, for clients that render subtitles with libass instead of
// a WebVTT <track>.
//
// The WebVTT path (ExtractSubtitleVTT) is lossy by construction: `-c:s webvtt`
// drops every override tag, so positioning, per-line styles, karaoke and typeset
// signs all disappear — and vector drawings leak through as literal path text
// (see vttfilter.go). Handing the ORIGINAL .ass to the client lets it render
// what the fansub author actually authored, the way VLC does.
//
// `-c:s copy` muxes the subtitle packets untouched; ffmpeg's ass muxer rebuilds
// the [Script Info] / [V4+ Styles] header from the stream's extradata, which in
// Matroska IS that header verbatim. Verified against a corpus of real fansub
// releases (Aegisub-muxed, pysubs2-generated and Erai-raws) — all three came
// back with PlayResX/Y and their full style table intact.

// ExtractSubtitleASS runs ffmpeg to copy subtitle stream `index` of mediaPath out
// as a raw ASS/SSA script. The caller must have checked the stream's codec is
// ass or ssa: `-f ass` on a subrip source would SYNTHESISE a default style
// table, producing a file that claims styling the author never wrote.
//
// The caller owns the ctx deadline, as with ExtractSubtitleVTT.
func ExtractSubtitleASS(ctx context.Context, ffmpegPath, mediaPath string, index int) ([]byte, error) {
	args := []string{
		"-nostdin",
		"-loglevel", "error",
		"-i", mediaPath,
		"-map", fmt.Sprintf("0:s:%d?", index),
		"-c:s", "copy",
		"-f", "ass",
		"-",
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	winproc.HideWindow(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg ass extract: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(out) == 0 {
		return nil, errors.New("ffmpeg produced no subtitle output")
	}
	return out, nil
}

// ReadExternalSubtitleASS returns a standalone .ass/.ssa sidecar as UTF-8 bytes.
//
// No ffmpeg: the file already IS the format we want to serve, so running it
// through the muxer would only risk losing sections it does not round-trip. The
// charset transcode still applies — fansub .ass files predating UTF-8 ubiquity
// are common, and libass would render mojibake otherwise.
func ReadExternalSubtitleASS(subPath, langHint string) ([]byte, error) {
	raw, err := os.ReadFile(subPath) //nolint:gosec // G304: path comes from a token-scoped, stat-validated sidecar discovered by the scanner.
	if err != nil {
		return nil, fmt.Errorf("read sidecar subtitle: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("sidecar subtitle is empty")
	}
	utf8Bytes, _ := DecodeSubtitleToUTF8(raw, langHint)
	return utf8Bytes, nil
}

// ReadCachedSubtitleASS returns the cached raw .ass for (mediaPath, index) when
// a fresh sidecar exists. ok=false means the caller should extract on demand.
func ReadCachedSubtitleASS(mediaPath string, index int) ([]byte, bool) {
	p := subtitleCachePathExt(mediaPath, index, "ass")
	if !sidecarFresh(p, mediaPath) {
		return nil, false
	}
	b, err := os.ReadFile(p) //nolint:gosec // G304: path is derived from mediaPath + a validated index.
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// WriteCachedSubtitleASS stores extracted .ass next to the media. Best-effort.
func WriteCachedSubtitleASS(mediaPath string, index int, ass []byte) error {
	return writeSidecar(subtitleCachePathExt(mediaPath, index, "ass"), ass)
}

// ExtractFontAttachment dumps font attachment `index` (ffmpeg's t:N ordering,
// see FontAttachment.Index) out of mediaPath and returns its bytes.
//
// ffmpeg has no "dump to stdout" mode for attachments, so this writes to a temp
// file and reads it back. It also EXITS NON-ZERO on success here: -dump_attachment
// does its work during input parsing and then ffmpeg complains "At least one
// output file must be specified" because the command specifies no output. The
// error is therefore only fatal when the dump produced nothing — verified
// against real MKVs, where the font lands on disk regardless of the exit code.
func ExtractFontAttachment(ctx context.Context, ffmpegPath, mediaPath string, index int, filename string) ([]byte, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		ext = ".ttf"
	}
	tmpDir, err := os.MkdirTemp("", "unarr-font-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	tmpOut := filepath.Join(tmpDir, "font"+ext)

	args := []string{
		"-nostdin",
		"-loglevel", "error",
		"-y",
		fmt.Sprintf("-dump_attachment:t:%d", index), tmpOut,
		"-i", mediaPath,
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	winproc.HideWindow(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// Judge by the artefact, not the exit code (see the doc comment).
	b, readErr := os.ReadFile(tmpOut) //nolint:gosec // G304: path is inside a temp dir we just created.
	if readErr != nil || len(b) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("ffmpeg dump_attachment: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return nil, errors.New("ffmpeg produced no font output")
	}
	return b, nil
}

// ReadCachedFont returns the cached font attachment for (mediaPath, index).
func ReadCachedFont(mediaPath string, index int, filename string) ([]byte, bool) {
	p := fontCachePath(mediaPath, index, strings.ToLower(filepath.Ext(filename)))
	if !sidecarFresh(p, mediaPath) {
		return nil, false
	}
	b, err := os.ReadFile(p) //nolint:gosec // G304: path is derived from mediaPath + a validated index.
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// WriteCachedFont stores a dumped font next to the media. Best-effort.
func WriteCachedFont(mediaPath string, index int, filename string, data []byte) error {
	return writeSidecar(fontCachePath(mediaPath, index, strings.ToLower(filepath.Ext(filename))), data)
}

// FontContentType maps a font filename to the MIME type to serve it as. Browsers
// do not sniff fonts, and libass only needs the bytes, but a correct type keeps
// devtools honest and avoids any chance of a security heuristic rejecting the
// response.
func FontContentType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".otf", ".otc":
		return "font/otf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttc":
		return "font/collection"
	default:
		return "font/ttf"
	}
}
