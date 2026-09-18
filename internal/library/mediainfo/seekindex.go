package mediainfo

// Container seek tables let COPY-VOD discover real boundaries without demuxing
// gigabytes of media. Reads are bounded, cached, and range-checked; unsupported
// or malformed indexes fail closed, never yield guessed timestamps.
import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const seekBlockSize = int64(64 * 1024)
const seekIndexBudget = int64(32 * 1024 * 1024)

var errSeekIndex = errors.New("unsupported or invalid container seek index")

type seekReader struct {
	ctx       context.Context
	file      *os.File
	url       string
	size      int64
	blocks    map[int64][]byte
	bytes     int64
	validator string
}

// ReadContainerKeyframes reads MP4 sample tables or Matroska Cues. It returns
// source presentation timestamps, as ffprobe does (not decode timestamps).
// A sparse cue table is fine: every returned point must be real, not every
// keyframe needs to be returned. No source or credential is included in errors.
func ReadContainerKeyframes(ctx context.Context, source string) ([]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	r := &seekReader{ctx: ctx, size: -1, blocks: make(map[int64][]byte)}
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		r.url = source
	} else {
		f, err := os.Open(source)
		if err != nil {
			return nil, errSeekIndex
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			return nil, errSeekIndex
		}
		r.file, r.size = f, fi.Size()
	}
	b, err := r.read(0, 8)
	if err != nil {
		return nil, err
	}
	var kfs []float64
	if string(b[:4]) == "\x1a\x45\xdf\xa3" {
		kfs, err = readMatroskaSeekIndex(r)
	} else {
		kfs, err = readMP4SeekIndex(r)
	}
	if err != nil {
		return nil, err
	}
	return normalizeSeekTimes(kfs)
}

func normalizeSeekTimes(kfs []float64) ([]float64, error) {
	if len(kfs) == 0 {
		return nil, errSeekIndex
	}
	sort.Float64s(kfs)
	out := kfs[:0]
	for _, t := range kfs {
		if math.IsNaN(t) || math.IsInf(t, 0) || t < 0 {
			return nil, errSeekIndex
		}
		if len(out) == 0 || t > out[len(out)-1] {
			out = append(out, t)
		}
	}
	return out, nil
}

func (r *seekReader) read(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || n > seekIndexBudget || off > math.MaxInt64-n || (r.size >= 0 && off+n > r.size) {
		return nil, errSeekIndex
	}
	result := make([]byte, n)
	for done := int64(0); done < n; {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		base := (off + done) / seekBlockSize * seekBlockSize
		b, ok := r.blocks[base]
		if !ok {
			var err error
			b, err = r.block(base)
			if err != nil {
				return nil, err
			}
			r.blocks[base] = b
		}
		pos := off + done - base
		if pos >= int64(len(b)) {
			return nil, io.ErrUnexpectedEOF
		}
		done += int64(copy(result[done:], b[pos:]))
	}
	return result, nil
}

func (r *seekReader) block(off int64) ([]byte, error) {
	if r.bytes+seekBlockSize > seekIndexBudget {
		return nil, errSeekIndex
	}
	n := seekBlockSize
	if r.size >= 0 && r.size-off < n {
		n = r.size - off
	}
	if n <= 0 {
		return nil, io.ErrUnexpectedEOF
	}
	r.bytes += n
	if r.file != nil {
		b := make([]byte, n)
		_, err := r.file.ReadAt(b, off)
		return b, err
	}
	return r.remoteBlock(off, n)
}

func (r *seekReader) remoteBlock(off, n int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, errSeekIndex
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "VLC/3.0.20 LibVLC/3.0.20")
	if r.validator != "" {
		req.Header.Set("If-Range", r.validator)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if r.ctx.Err() != nil {
			return nil, r.ctx.Err()
		}
		return nil, errSeekIndex
	}
	defer resp.Body.Close()
	length, err := r.validateRange(resp, off, n)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, length+1))
	if err != nil || int64(len(b)) != length {
		return nil, errSeekIndex
	}
	return b, nil
}

func (r *seekReader) validateRange(resp *http.Response, off, n int64) (int64, error) {
	var first, last, size int64
	_, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &size)
	if resp.StatusCode != http.StatusPartialContent || err != nil || first != off || last < first || last-first+1 > n || size <= last || (r.size >= 0 && size != r.size) || resp.Header.Get("Content-Encoding") != "" {
		return 0, errSeekIndex
	}
	validator := resp.Header.Get("ETag")
	if strings.HasPrefix(validator, "W/") {
		validator = ""
	}
	if validator == "" {
		validator = resp.Header.Get("Last-Modified")
	}
	if r.validator != "" && validator != r.validator {
		return 0, errSeekIndex
	}
	r.validator, r.size = validator, size
	return last - first + 1, nil
}
