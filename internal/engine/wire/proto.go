// Package wire implements the binary frame format used over the WebRTC
// DataChannel between the unarr daemon and the browser stream player.
//
// Header (12 bytes, big-endian):
//
//	u8  Type
//	u8  Flags
//	u16 _reserved
//	u32 StreamID  -- multiplex range requests
//	u32 Length    -- payload bytes following the header
//
// Each side encodes one Frame at a time and writes it as a single SCTP
// message (DataChannel send). Browsers cap message size at 64 KiB-ish, so
// callers MUST split RANGE_DATA payloads into chunks <= MaxChunkPayload.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FrameType identifies the wire message kind.
type FrameType uint8

const (
	FrameHello     FrameType = 0x00
	FrameRangeReq  FrameType = 0x01
	FrameRangeData FrameType = 0x02
	FrameRangeEnd  FrameType = 0x03
	FrameCancel    FrameType = 0x04
	FramePing      FrameType = 0x05
	FramePong      FrameType = 0x06
	FrameSeekHint  FrameType = 0x07
)

// Flag bits — interpretation depends on FrameType.
const (
	// FlagLastChunk on a RangeData frame marks the final chunk for a stream_id.
	FlagLastChunk uint8 = 1 << 0
	// FlagTranscoding on a Hello frame indicates the daemon will transcode.
	FlagTranscoding uint8 = 1 << 1
	// FlagSeekable on a Hello frame indicates random-access is supported.
	FlagSeekable uint8 = 1 << 2
)

// HeaderSize is the fixed length of every frame header.
const HeaderSize = 12

// MaxChunkPayload is the safe per-frame payload cap that works on every
// browser implementation (Chromium fragments at 16 KiB internally above).
// Callers MUST chunk RangeData payloads to <= this size.
const MaxChunkPayload = 16 * 1024

// MaxFrameSize is the largest frame the parser will accept. Anything bigger
// is treated as a corrupted stream — close the channel.
const MaxFrameSize = HeaderSize + 64*1024

// Header is the parsed 12-byte frame header.
type Header struct {
	Type     FrameType
	Flags    uint8
	StreamID uint32
	Length   uint32
}

// EncodeHeader writes h to dst (must be at least HeaderSize bytes).
func EncodeHeader(dst []byte, h Header) {
	if len(dst) < HeaderSize {
		panic("wire: dst too small for header")
	}
	dst[0] = byte(h.Type)
	dst[1] = h.Flags
	dst[2] = 0
	dst[3] = 0
	binary.BigEndian.PutUint32(dst[4:8], h.StreamID)
	binary.BigEndian.PutUint32(dst[8:12], h.Length)
}

// DecodeHeader parses src (must be at least HeaderSize bytes) into h.
func DecodeHeader(src []byte) (Header, error) {
	if len(src) < HeaderSize {
		return Header{}, fmt.Errorf("wire: header needs %d bytes, got %d", HeaderSize, len(src))
	}
	h := Header{
		Type:     FrameType(src[0]),
		Flags:    src[1],
		StreamID: binary.BigEndian.Uint32(src[4:8]),
		Length:   binary.BigEndian.Uint32(src[8:12]),
	}
	if h.Length > MaxFrameSize-HeaderSize {
		return Header{}, fmt.Errorf("wire: payload length %d exceeds max %d", h.Length, MaxFrameSize-HeaderSize)
	}
	return h, nil
}

// EncodeFrame allocates and returns a complete frame (header + payload).
// Use this for one-shot sends; for hot-path RangeData prefer EncodeHeader
// into a pre-allocated buffer to avoid per-frame allocations.
func EncodeFrame(h Header, payload []byte) []byte {
	if int(h.Length) != len(payload) {
		panic(fmt.Sprintf("wire: header length %d != payload len %d", h.Length, len(payload)))
	}
	buf := make([]byte, HeaderSize+len(payload))
	EncodeHeader(buf[:HeaderSize], h)
	copy(buf[HeaderSize:], payload)
	return buf
}

// ReadFrame reads one full frame from r. Returns the parsed header and a
// freshly allocated payload slice. On any size violation the connection
// must be closed — the protocol has no resync.
func ReadFrame(r io.Reader) (Header, []byte, error) {
	headerBuf := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, headerBuf); err != nil {
		return Header{}, nil, err
	}
	h, err := DecodeHeader(headerBuf)
	if err != nil {
		return Header{}, nil, err
	}
	if h.Length == 0 {
		return h, nil, nil
	}
	payload := make([]byte, h.Length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Header{}, nil, err
	}
	return h, payload, nil
}

// HelloPayload describes the file the daemon is about to serve. It is the
// first frame the daemon writes after the DataChannel opens.
type HelloPayload struct {
	FileSize    uint64
	Transcoding bool
	Seekable    bool
	FileName    string
}

// EncodeHello marshals h into a payload byte slice.
//
// Layout: u64 file_size | u32 name_len | name_bytes
func EncodeHello(h HelloPayload) []byte {
	nameBytes := []byte(h.FileName)
	buf := make([]byte, 8+4+len(nameBytes))
	binary.BigEndian.PutUint64(buf[0:8], h.FileSize)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(nameBytes)))
	copy(buf[12:], nameBytes)
	return buf
}

// DecodeHello parses a Hello payload. The transcoding/seekable bits live in
// the frame Flags byte, not the payload — pass them in.
func DecodeHello(payload []byte, flags uint8) (HelloPayload, error) {
	if len(payload) < 12 {
		return HelloPayload{}, errors.New("wire: hello payload too short")
	}
	size := binary.BigEndian.Uint64(payload[0:8])
	nameLen := binary.BigEndian.Uint32(payload[8:12])
	if int(nameLen) > len(payload)-12 {
		return HelloPayload{}, fmt.Errorf("wire: hello name_len %d exceeds payload", nameLen)
	}
	return HelloPayload{
		FileSize:    size,
		Transcoding: flags&FlagTranscoding != 0,
		Seekable:    flags&FlagSeekable != 0,
		FileName:    string(payload[12 : 12+nameLen]),
	}, nil
}

// HelloFlags returns the flag byte for a Hello frame given the booleans.
func HelloFlags(transcoding, seekable bool) uint8 {
	var f uint8
	if transcoding {
		f |= FlagTranscoding
	}
	if seekable {
		f |= FlagSeekable
	}
	return f
}

// RangeReqPayload is the browser → daemon request for bytes [Offset, Offset+Length).
type RangeReqPayload struct {
	Offset uint64
	Length uint64
}

// EncodeRangeReq marshals p. Layout: u64 offset | u64 length.
func EncodeRangeReq(p RangeReqPayload) []byte {
	buf := make([]byte, 16)
	binary.BigEndian.PutUint64(buf[0:8], p.Offset)
	binary.BigEndian.PutUint64(buf[8:16], p.Length)
	return buf
}

// DecodeRangeReq parses a 16-byte range request payload.
func DecodeRangeReq(payload []byte) (RangeReqPayload, error) {
	if len(payload) != 16 {
		return RangeReqPayload{}, fmt.Errorf("wire: range_req payload must be 16 bytes, got %d", len(payload))
	}
	return RangeReqPayload{
		Offset: binary.BigEndian.Uint64(payload[0:8]),
		Length: binary.BigEndian.Uint64(payload[8:16]),
	}, nil
}

// RangeEndPayload signals end-of-response for a stream_id with a status code.
// Status 0 == OK; non-zero values are app-defined error codes.
type RangeEndPayload struct {
	Status uint32
}

// EncodeRangeEnd marshals p.
func EncodeRangeEnd(p RangeEndPayload) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf[0:4], p.Status)
	return buf
}

// DecodeRangeEnd parses a 4-byte range_end payload.
func DecodeRangeEnd(payload []byte) (RangeEndPayload, error) {
	if len(payload) != 4 {
		return RangeEndPayload{}, fmt.Errorf("wire: range_end payload must be 4 bytes, got %d", len(payload))
	}
	return RangeEndPayload{
		Status: binary.BigEndian.Uint32(payload[0:4]),
	}, nil
}

// SeekHintPayload tells the daemon a seek to timestamp_ms is imminent so it
// can pre-warm a transcoder pipeline before bytes are requested.
type SeekHintPayload struct {
	TimestampMs uint64
}

// EncodeSeekHint marshals p.
func EncodeSeekHint(p SeekHintPayload) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf[0:8], p.TimestampMs)
	return buf
}

// DecodeSeekHint parses an 8-byte seek_hint payload.
func DecodeSeekHint(payload []byte) (SeekHintPayload, error) {
	if len(payload) != 8 {
		return SeekHintPayload{}, fmt.Errorf("wire: seek_hint payload must be 8 bytes, got %d", len(payload))
	}
	return SeekHintPayload{
		TimestampMs: binary.BigEndian.Uint64(payload[0:8]),
	}, nil
}
