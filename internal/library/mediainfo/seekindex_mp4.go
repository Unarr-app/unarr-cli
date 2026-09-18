package mediainfo

import (
	"encoding/binary"
	"math"
)

type mp4Box struct {
	kind      string
	data, end int64
}

func (r *seekReader) mp4Boxes(from, to int64) ([]mp4Box, error) {
	var boxes []mp4Box
	for off := from; off < to; {
		if len(boxes) >= 10000 || to-off < 8 {
			return nil, errSeekIndex
		}
		b, err := r.read(off, 8)
		if err != nil {
			return nil, err
		}
		size, header := uint64(binary.BigEndian.Uint32(b)), int64(8)
		kind := string(b[4:8])
		if size == 1 {
			b, err = r.read(off+8, 8)
			if err != nil {
				return nil, err
			}
			size, header = binary.BigEndian.Uint64(b), 16
		} else if size == 0 {
			size = uint64(to - off)
		}
		if size < uint64(header) || size > uint64(to-off) {
			return nil, errSeekIndex
		}
		boxes = append(boxes, mp4Box{kind, off + header, off + int64(size)})
		off += int64(size)
	}
	return boxes, nil
}

func (r *seekReader) mp4Child(parent mp4Box, kind string) (mp4Box, error) {
	boxes, err := r.mp4Boxes(parent.data, parent.end)
	if err != nil {
		return mp4Box{}, err
	}
	for _, b := range boxes {
		if b.kind == kind {
			return b, nil
		}
	}
	return mp4Box{}, errSeekIndex
}

func (r *seekReader) mp4Data(parent mp4Box, kind string) ([]byte, error) {
	b, err := r.mp4Child(parent, kind)
	if err != nil {
		return nil, err
	}
	return r.read(b.data, b.end-b.data)
}

func mp4Timescale(b []byte) (uint32, error) {
	if len(b) < 20 {
		return 0, errSeekIndex
	}
	off := 12
	if b[0] == 1 {
		off = 20
	} else if b[0] != 0 {
		return 0, errSeekIndex
	}
	if len(b) < off+4 {
		return 0, errSeekIndex
	}
	scale := binary.BigEndian.Uint32(b[off:])
	if scale == 0 {
		return 0, errSeekIndex
	}
	return scale, nil
}

type mp4TimeRun struct {
	count uint32
	value int64
}

func mp4Runs(b []byte, composition bool) ([]mp4TimeRun, error) {
	if len(b) < 8 || b[0] > 1 || (!composition && b[0] != 0) {
		return nil, errSeekIndex
	}
	n := binary.BigEndian.Uint32(b[4:])
	if uint64(n)*8 != uint64(len(b)-8) {
		return nil, errSeekIndex
	}
	runs := make([]mp4TimeRun, n)
	for i := range runs {
		count := binary.BigEndian.Uint32(b[8+i*8:])
		v := binary.BigEndian.Uint32(b[12+i*8:])
		if count == 0 {
			return nil, errSeekIndex
		}
		runs[i] = mp4TimeRun{count, int64(v)}
		if composition && b[0] == 1 {
			runs[i].value = int64(int32(v))
		}
	}
	return runs, nil
}

// Read sync samples on the presentation timeline: stts + ctts + elst. Ignoring
// ctts produces decode timestamps; ignoring elst offsets every cut on normal
// B-frame MP4s. Fragmented files and complex/repeating edits are not guessed.
func readMP4SeekIndex(r *seekReader) ([]float64, error) {
	root := mp4Box{"root", 0, r.size}
	moov, err := r.mp4Child(root, "moov")
	if err != nil {
		return nil, err
	}
	if b, _ := r.mp4Child(moov, "mvex"); b.kind != "" {
		return nil, errSeekIndex
	}
	mvhd, err := r.mp4Data(moov, "mvhd")
	if err != nil {
		return nil, err
	}
	movieScale, err := mp4Timescale(mvhd)
	if err != nil {
		return nil, err
	}
	tracks, err := r.mp4Boxes(moov.data, moov.end)
	if err != nil {
		return nil, err
	}
	for _, track := range tracks {
		if track.kind != "trak" {
			continue
		}
		mdia, err := r.mp4Child(track, "mdia")
		if err != nil {
			return nil, err
		}
		hdlr, err := r.mp4Data(mdia, "hdlr")
		if err != nil || len(hdlr) < 12 {
			return nil, errSeekIndex
		}
		if string(hdlr[8:12]) != "vide" {
			continue
		}
		return r.mp4TrackTimes(track, mdia, movieScale)
	}
	return nil, errSeekIndex
}

func (r *seekReader) mp4TrackTimes(track, mdia mp4Box, movieScale uint32) ([]float64, error) {
	mdhd, err := r.mp4Data(mdia, "mdhd")
	if err != nil {
		return nil, err
	}
	scale, err := mp4Timescale(mdhd)
	if err != nil {
		return nil, err
	}
	shift, err := r.mp4EditShift(track, movieScale, scale)
	if err != nil {
		return nil, err
	}
	minf, err := r.mp4Child(mdia, "minf")
	if err != nil {
		return nil, err
	}
	stbl, err := r.mp4Child(minf, "stbl")
	if err != nil {
		return nil, err
	}
	return r.mp4SyncTimes(stbl, scale, shift)
}

func (r *seekReader) mp4EditShift(track mp4Box, movieScale, scale uint32) (float64, error) {
	edts, _ := r.mp4Child(track, "edts")
	if edts.kind == "" {
		return 0, nil
	}
	b, err := r.mp4Data(edts, "elst")
	if err != nil || len(b) < 8 || b[0] > 1 {
		return 0, errSeekIndex
	}
	return mp4EditListShift(b, movieScale, scale)
}

func mp4EditListShift(b []byte, movieScale, scale uint32) (float64, error) {
	n, stride := binary.BigEndian.Uint32(b[4:]), 12
	if b[0] == 1 {
		stride = 20
	}
	if n < 1 || n > 2 || len(b) != 8+int(n)*stride {
		return 0, errSeekIndex
	}
	shift := 0.0
	for i := 0; i < int(n); i++ {
		dur, media, err := mp4EditEntry(b[8+i*stride:], b[0])
		if err != nil {
			return 0, err
		}
		if media == -1 && i == 0 && n == 2 {
			shift += float64(dur) / float64(movieScale)
			continue
		}
		if media < 0 || i != int(n)-1 {
			return 0, errSeekIndex
		}
		shift -= float64(media) / float64(scale)
	}
	return shift, nil
}

func mp4EditEntry(p []byte, version byte) (uint64, int64, error) {
	dur, media, rate := uint64(binary.BigEndian.Uint32(p)), int64(int32(binary.BigEndian.Uint32(p[4:]))), 8
	if version == 1 {
		dur, media, rate = binary.BigEndian.Uint64(p), int64(binary.BigEndian.Uint64(p[8:])), 16
	}
	if binary.BigEndian.Uint32(p[rate:]) != 0x10000 {
		return 0, 0, errSeekIndex
	}
	return dur, media, nil
}

func (r *seekReader) mp4SyncTimes(stbl mp4Box, scale uint32, shift float64) ([]float64, error) {
	b, err := r.mp4Data(stbl, "stts")
	if err != nil {
		return nil, err
	}
	runs, err := mp4Runs(b, false)
	if err != nil {
		return nil, err
	}
	var total uint64
	for _, run := range runs {
		total += uint64(run.count)
	}
	if total == 0 || total > 20000000 {
		return nil, errSeekIndex
	}
	composition, err := r.mp4Composition(stbl, total)
	if err != nil {
		return nil, err
	}
	sync, err := r.mp4SyncSamples(stbl, total)
	if err != nil {
		return nil, err
	}
	return mp4PresentationTimes(runs, composition, sync, scale, shift), nil
}

func (r *seekReader) mp4Composition(stbl mp4Box, total uint64) ([]mp4TimeRun, error) {
	ctts, _ := r.mp4Child(stbl, "ctts")
	if ctts.kind == "" {
		return []mp4TimeRun{{uint32(total), 0}}, nil
	}
	b, err := r.read(ctts.data, ctts.end-ctts.data)
	if err != nil {
		return nil, err
	}
	composition, err := mp4Runs(b, true)
	if err != nil {
		return nil, err
	}
	var n uint64
	for _, run := range composition {
		n += uint64(run.count)
	}
	if n != total {
		return nil, errSeekIndex
	}
	return composition, nil
}

func (r *seekReader) mp4SyncSamples(stbl mp4Box, total uint64) ([]uint32, error) {
	stss, _ := r.mp4Child(stbl, "stss")
	if stss.kind == "" {
		if total > 1000000 {
			return nil, errSeekIndex
		}
		return nil, nil
	}
	b, err := r.read(stss.data, stss.end-stss.data)
	if err != nil || len(b) < 8 || b[0] != 0 {
		return nil, errSeekIndex
	}
	n := binary.BigEndian.Uint32(b[4:])
	if n == 0 || n > 1000000 || uint64(n)*4 != uint64(len(b)-8) {
		return nil, errSeekIndex
	}
	var sync []uint32
	var previous uint32
	for i := uint32(0); i < n; i++ {
		sample := binary.BigEndian.Uint32(b[8+4*i:])
		if sample <= previous || uint64(sample) > total {
			return nil, errSeekIndex
		}
		sync = append(sync, sample)
		previous = sample
	}
	return sync, nil
}

type mp4RunCursor struct {
	runs []mp4TimeRun
	left uint32
}

func (c *mp4RunCursor) advance() {
	c.left--
	if c.left == 0 {
		c.runs = c.runs[1:]
		if len(c.runs) > 0 {
			c.left = c.runs[0].count
		}
	}
}

func mp4PresentationTimes(runs, composition []mp4TimeRun, sync []uint32, scale uint32, shift float64) []float64 {
	var result []float64
	var dts int64
	si := 0
	decode := mp4RunCursor{runs, runs[0].count}
	present := mp4RunCursor{composition, composition[0].count}
	for sample := uint32(1); len(decode.runs) > 0; sample++ {
		if sync == nil || (si < len(sync) && sample == sync[si]) {
			t := float64(dts+present.runs[0].value)/float64(scale) + shift
			if t >= -0.000001 {
				result = append(result, math.Max(0, t))
			}
			si++
		}
		dts += decode.runs[0].value
		decode.advance()
		present.advance()
	}
	return result
}
