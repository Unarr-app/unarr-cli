package mediainfo

import "encoding/binary"

type ebmlElement struct {
	id        uint64
	data, end int64
}

type ebmlVint struct {
	value   uint64
	next    int64
	unknown bool
}

func (r *seekReader) ebmlVintAt(off, limit int64, id bool) (ebmlVint, error) {
	b, err := r.read(off, 1)
	if err != nil {
		return ebmlVint{}, err
	}
	n, mask := int64(1), byte(0x80)
	for mask != 0 && b[0]&mask == 0 {
		n++
		mask >>= 1
	}
	if mask == 0 || (id && n > 4) || off+n > limit {
		return ebmlVint{}, errSeekIndex
	}
	b, err = r.read(off, n)
	if err != nil {
		return ebmlVint{}, err
	}
	v := uint64(b[0] & (mask - 1))
	if id {
		v = uint64(b[0])
	}
	for _, c := range b[1:] {
		v = v<<8 | uint64(c)
	}
	return ebmlVint{v, off + n, !id && v == (uint64(1)<<(7*n))-1}, nil
}

func (r *seekReader) ebml(off, limit int64) (ebmlElement, error) {
	id, err := r.ebmlVintAt(off, limit, true)
	if err != nil {
		return ebmlElement{}, err
	}
	size, err := r.ebmlVintAt(id.next, limit, false)
	if err != nil {
		return ebmlElement{}, err
	}
	end := limit
	if !size.unknown {
		if size.value > uint64(limit-size.next) {
			return ebmlElement{}, errSeekIndex
		}
		end = size.next + int64(size.value)
	} else if id.value != 0x18538067 {
		return ebmlElement{}, errSeekIndex
	}
	return ebmlElement{id.value, size.next, end}, nil
}

func (r *seekReader) ebmlChildren(parent ebmlElement, fn func(ebmlElement) error) error {
	for off, count := parent.data, 0; off < parent.end; count++ {
		if count > 1000000 {
			return errSeekIndex
		}
		el, err := r.ebml(off, parent.end)
		if err != nil {
			return err
		}
		if err := fn(el); err != nil {
			return err
		}
		off = el.end
	}
	return nil
}

func (r *seekReader) ebmlUint(el ebmlElement) (uint64, error) {
	n := el.end - el.data
	if n < 1 || n > 8 {
		return 0, errSeekIndex
	}
	b, err := r.read(el.data, n)
	if err != nil {
		return 0, err
	}
	var padded [8]byte
	copy(padded[8-n:], b)
	return binary.BigEndian.Uint64(padded[:]), nil
}

// Cues are absolute Segment timestamps. Select only the first video track,
// follow SeekHead rather than reading Clusters, and honour TimestampScale.
// TrackTimestampScale/CodecDelay are deliberately rejected until supported.
func readMatroskaSeekIndex(r *seekReader) ([]float64, error) {
	header, err := r.ebml(0, r.size)
	if err != nil {
		return nil, err
	}
	segment, err := r.ebml(header.end, r.size)
	if err != nil || segment.id != 0x18538067 {
		return nil, errSeekIndex
	}
	index := matroskaSeekLocations{r, segment, make(map[uint64]ebmlElement), make(map[int64]bool)}
	if err := index.locate(); err != nil {
		return nil, err
	}
	info, tracks, cues := index.locations[0x1549a966], index.locations[0x1654ae6b], index.locations[0x1c53bb6b]
	if info.id == 0 || tracks.id == 0 || cues.id == 0 {
		return nil, errSeekIndex
	}
	scale, err := r.matroskaScale(info)
	if err != nil {
		return nil, err
	}
	video, err := r.matroskaVideoTrack(tracks)
	if err != nil {
		return nil, err
	}
	return r.matroskaCueTimes(cues, video, scale)
}

type matroskaSeekLocations struct {
	r         *seekReader
	segment   ebmlElement
	locations map[uint64]ebmlElement
	visited   map[int64]bool
}

func (s *matroskaSeekLocations) seek(head ebmlElement) error {
	if s.visited[head.data] {
		return nil
	}
	if len(s.visited) >= 8 {
		return errSeekIndex
	}
	s.visited[head.data] = true
	return s.r.ebmlChildren(head, func(entry ebmlElement) error {
		if entry.id != 0x4dbb {
			return nil
		}
		el, err := s.entry(entry)
		if err != nil || el.id == 0 {
			return err
		}
		s.locations[el.id] = el
		if el.id == 0x114d9b74 {
			return s.seek(el)
		}
		return nil
	})
}

func (s *matroskaSeekLocations) entry(entry ebmlElement) (ebmlElement, error) {
	var id, pos uint64
	err := s.r.ebmlChildren(entry, func(e ebmlElement) error {
		var err error
		switch e.id {
		case 0x53ab:
			id, err = s.r.ebmlUint(e)
		case 0x53ac:
			pos, err = s.r.ebmlUint(e)
		}
		return err
	})
	if err != nil {
		return ebmlElement{}, err
	}
	if id != 0x1549a966 && id != 0x1654ae6b && id != 0x1c53bb6b && id != 0x114d9b74 {
		return ebmlElement{}, nil
	}
	if pos >= uint64(s.segment.end-s.segment.data) {
		return ebmlElement{}, errSeekIndex
	}
	el, err := s.r.ebml(s.segment.data+int64(pos), s.segment.end)
	if err != nil || el.id != id {
		return ebmlElement{}, errSeekIndex
	}
	return el, nil
}

func (s *matroskaSeekLocations) locate() error {
	// Without a SeekHead, walk level-1 headers (skip payloads), under the same
	// read budget. Never scan arbitrary bytes looking for an element signature.
	for off, count := s.segment.data, 0; off < s.segment.end; count++ {
		if count > 4096 {
			return errSeekIndex
		}
		el, err := s.r.ebml(off, s.segment.end)
		if err != nil {
			return err
		}
		s.locations[el.id] = el
		if el.id == 0x114d9b74 {
			if err := s.seek(el); err != nil {
				return err
			}
		}
		if s.locations[0x1549a966].id != 0 && s.locations[0x1654ae6b].id != 0 && s.locations[0x1c53bb6b].id != 0 {
			break
		}
		off = el.end
	}
	return nil
}

func (r *seekReader) matroskaScale(info ebmlElement) (uint64, error) {
	scale := uint64(1000000)
	if err := r.ebmlChildren(info, func(el ebmlElement) error {
		if el.id == 0x2ad7b1 {
			var err error
			scale, err = r.ebmlUint(el)
			return err
		}
		return nil
	}); err != nil || scale == 0 {
		return 0, errSeekIndex
	}
	return scale, nil
}

func (r *seekReader) matroskaVideoTrack(tracks ebmlElement) (uint64, error) {
	var video uint64
	err := r.ebmlChildren(tracks, func(track ebmlElement) error {
		if track.id != 0xae || video != 0 {
			return nil
		}
		var err error
		video, err = r.matroskaTrackNumber(track)
		return err
	})
	if err != nil || video == 0 {
		return 0, errSeekIndex
	}
	return video, nil
}

func (r *seekReader) matroskaTrackNumber(track ebmlElement) (uint64, error) {
	var num, kind uint64
	unsupported := false
	err := r.ebmlChildren(track, func(el ebmlElement) error {
		var err error
		switch el.id {
		case 0xd7:
			num, err = r.ebmlUint(el)
		case 0x83:
			kind, err = r.ebmlUint(el)
		case 0x23314f, 0x56aa:
			unsupported = true
		}
		return err
	})
	if kind != 1 {
		return 0, err
	}
	if unsupported || num == 0 {
		return 0, errSeekIndex
	}
	return num, err
}

func (r *seekReader) matroskaCueTimes(cues ebmlElement, video, scale uint64) ([]float64, error) {
	var result []float64
	err := r.ebmlChildren(cues, func(point ebmlElement) error {
		if point.id != 0xbb {
			return nil
		}
		stamp, selected, err := r.matroskaCuePoint(point, video)
		if err != nil {
			return err
		}
		if selected {
			result = append(result, float64(stamp)*float64(scale)/1e9)
		}
		return nil
	})
	return result, err
}

func (r *seekReader) matroskaCuePoint(point ebmlElement, video uint64) (uint64, bool, error) {
	var stamp uint64
	hasTime, selected := false, false
	err := r.ebmlChildren(point, func(el ebmlElement) error {
		if el.id == 0xb3 {
			var err error
			stamp, err = r.ebmlUint(el)
			hasTime = true
			return err
		}
		if el.id == 0xb7 {
			return r.ebmlChildren(el, func(p ebmlElement) error {
				if p.id != 0xf7 {
					return nil
				}
				tr, err := r.ebmlUint(p)
				selected = selected || tr == video
				return err
			})
		}
		return nil
	})
	return stamp, hasTime && selected, err
}
