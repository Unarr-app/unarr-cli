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

// ErrNotStyledSubtitle means the extracted script carries no authored styling,
// so serving it as an ASS original would be a lie. Callers fall back to WebVTT.
var ErrNotStyledSubtitle = errors.New("subtitle stream carries no ASS styling")

// ExtractSubtitleASS runs ffmpeg to copy subtitle stream `index` of mediaPath out
// as a raw ASS/SSA script.
//
// `-f ass` on a NON-ass source (subrip, mov_text) must DECLINE, not fabricate:
// the URL layer only advertises assUrl for ass/ssa tracks, but the /sub token
// deliberately does not bind the serialisation, so anyone holding a token for a
// subrip track can still ask for `f=ass`. How ffmpeg reacts depends on version:
// ffmpeg 8's ass muxer refuses `-c:s copy` of a non-ass codec outright ("ass
// muxer supports only codec ass"), which is mapped to ErrNotStyledSubtitle
// below; older ffmpeg synthesised a lone "Default" style when TRANSCODING, so
// the OUTPUT is additionally checked: a genuine ASS original carries an
// authored style table (see hasAuthoredStyles). The content check doubles as
// the guard for an exotic mux whose extradata failed to round-trip.
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
		if isAssMuxerRefusal(stderr.String()) {
			return nil, ErrNotStyledSubtitle
		}
		return nil, fmt.Errorf("ffmpeg ass extract: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(out) == 0 {
		return nil, errors.New("ffmpeg produced no subtitle output")
	}
	if !hasAuthoredStyles(out) {
		return nil, ErrNotStyledSubtitle
	}
	return out, nil
}

// hasAuthoredStyles reports whether an ASS script carries a real style table —
// the marker that separates an author's script from one ffmpeg synthesised while
// muxing a plain-text subtitle into the ass container.
func hasAuthoredStyles(ass []byte) bool {
	s := string(ass)
	i := strings.Index(s, "[V4+ Styles]")
	if i < 0 {
		i = strings.Index(s, "[V4 Styles]")
	}
	if i < 0 {
		return false
	}
	// A synthesised table names its lone style "Default"; an authored one names
	// its own (fansubs ship Main/Sign_Basic/Italics/… — the production corpus had
	// 13). Requiring either a second Style line or a non-Default name keeps the
	// rare single-style authored script while rejecting ffmpeg's stand-in.
	rest := s[i:]
	if end := strings.Index(rest, "\n["); end > 0 {
		rest = rest[:end]
	}
	styles := 0
	authored := false
	for _, line := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Style:") {
			continue
		}
		styles++
		name := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(line), "Style:"), ",", 2)[0])
		if !strings.EqualFold(name, "Default") {
			authored = true
		}
	}
	return styles > 1 || authored
}

// IsASSSubtitleCodec reports whether a probed subtitle codec is ASS/SSA — the
// only codecs whose original script `-c:s copy -f ass` can round-trip. Shared
// by the scan-time prewarm and the engine's URL layer so both classify
// identically.
func IsASSSubtitleCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "ass", "ssa":
		return true
	default:
		return false
	}
}

// IsASSSubtitlePath reports whether an EXTERNAL sidecar file is an ASS/SSA
// script, by extension. The extension is the honest signal here: sidecars are
// author-named files, and serving a .srt's bytes under `f=ass` would hand a
// libass client an unparseable "script" (200 + no subtitles, no error).
func IsASSSubtitlePath(subPath string) bool {
	switch strings.ToLower(filepath.Ext(subPath)) {
	case ".ass", ".ssa":
		return true
	default:
		return false
	}
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
	ext := SafeFontExt(filename)
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

	// A dump that was KILLED mid-write (ctx deadline, or the client hanging up —
	// the handler's ctx derives from the request) leaves a PARTIAL font on disk.
	// ffmpeg writes the attachment incrementally, so that file is non-empty,
	// would pass the size check below, and the handler would then cache it for a
	// day — a corrupt font served forever. Distrust everything if the run was
	// aborted; the only benign non-zero exit is the documented "at least one
	// output file" complaint, which happens AFTER the dump completed.
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ffmpeg dump_attachment aborted (%v): %s",
			ctx.Err(), strings.TrimSpace(stderr.String()))
	}
	var exitErr *exec.ExitError
	if runErr != nil && errors.As(runErr, &exitErr) && !exitErr.ProcessState.Exited() {
		return nil, fmt.Errorf("ffmpeg dump_attachment killed (%v): %s",
			runErr, strings.TrimSpace(stderr.String()))
	}

	// Judge by the artefact, not the exit code (see the doc comment).
	b, readErr := os.ReadFile(tmpOut) //nolint:gosec // G304: path is inside a temp dir we just created.
	if readErr != nil || len(b) == 0 {
		// Always surface stderr: this command exits non-zero even when it works,
		// so the runErr==nil path is the unusual one and needs the message most.
		return nil, fmt.Errorf("ffmpeg dump_attachment produced no font (exit=%v): %s",
			runErr, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

// ReadCachedFont returns the cached font attachment for (mediaPath, index).
func ReadCachedFont(mediaPath string, index int, filename string) ([]byte, bool) {
	p := fontCachePath(mediaPath, index, SafeFontExt(filename))
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
	return writeSidecar(fontCachePath(mediaPath, index, SafeFontExt(filename)), data)
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

// isAssMuxerRefusal recognises ffmpeg's ass muxer declining to `-c:s copy` a
// non-ass stream. That is the NORMAL decline for a subrip/mov_text track, not
// an operational error — but the wording is not stable across ffmpeg builds:
// 8.0 says "ass muxer supports only codec ass", newer static builds say
// "Exactly one ASS/SSA stream is needed". Either way, the muxer is telling us
// the stream is not ASS, which is exactly ErrNotStyledSubtitle.
func isAssMuxerRefusal(stderr string) bool {
	return strings.Contains(stderr, "ass muxer supports only codec") ||
		strings.Contains(stderr, "Exactly one ASS/SSA stream is needed")
}
