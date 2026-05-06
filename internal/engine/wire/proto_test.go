package wire

import (
	"bytes"
	"testing"
)

func TestHeaderRoundtrip(t *testing.T) {
	cases := []Header{
		{Type: FrameHello, Flags: FlagSeekable, StreamID: 0, Length: 32},
		{Type: FrameRangeReq, Flags: 0, StreamID: 7, Length: 16},
		{Type: FrameRangeData, Flags: FlagLastChunk, StreamID: 4242, Length: 16380},
		{Type: FrameRangeEnd, Flags: 0, StreamID: 1, Length: 4},
		{Type: FrameCancel, Flags: 0, StreamID: 9, Length: 0},
		{Type: FramePing, Flags: 0, StreamID: 0, Length: 0},
	}
	for _, want := range cases {
		buf := make([]byte, HeaderSize)
		EncodeHeader(buf, want)
		got, err := DecodeHeader(buf)
		if err != nil {
			t.Fatalf("decode: %v (want %+v)", err, want)
		}
		if got != want {
			t.Errorf("roundtrip mismatch: got %+v want %+v", got, want)
		}
	}
}

func TestDecodeHeaderShort(t *testing.T) {
	if _, err := DecodeHeader([]byte{0, 0, 0}); err == nil {
		t.Fatal("expected error on short header")
	}
}

func TestDecodeHeaderRejectsHugeLength(t *testing.T) {
	// Synthesize a header with payload length above MaxFrameSize.
	buf := make([]byte, HeaderSize)
	buf[0] = byte(FrameRangeData)
	buf[8] = 0xff
	buf[9] = 0xff
	buf[10] = 0xff
	buf[11] = 0xff
	if _, err := DecodeHeader(buf); err == nil {
		t.Fatal("expected error on oversized payload length")
	}
}

func TestEncodeFramePanicsOnLengthMismatch(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on header length / payload mismatch")
		}
	}()
	EncodeFrame(Header{Type: FrameRangeData, Length: 5}, []byte{1, 2, 3})
}

func TestReadFrameRoundtrip(t *testing.T) {
	want := Header{Type: FrameRangeData, Flags: FlagLastChunk, StreamID: 99, Length: 5}
	payload := []byte{0xde, 0xad, 0xbe, 0xef, 0x42}
	frame := EncodeFrame(want, payload)

	r := bytes.NewReader(frame)
	got, gotPayload, err := ReadFrame(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Errorf("header mismatch: %+v want %+v", got, want)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload mismatch: %x want %x", gotPayload, payload)
	}
}

func TestReadFrameZeroPayload(t *testing.T) {
	want := Header{Type: FrameCancel, StreamID: 7}
	frame := EncodeFrame(want, nil)
	got, payload, err := ReadFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Errorf("header mismatch: %+v want %+v", got, want)
	}
	if len(payload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(payload))
	}
}

func TestHelloRoundtrip(t *testing.T) {
	want := HelloPayload{
		FileSize:    1<<32 + 12345,
		Transcoding: false,
		Seekable:    true,
		FileName:    "Tangled.Ever.After.2025.1080p.WEB-DL.h264.mp4",
	}
	flags := HelloFlags(want.Transcoding, want.Seekable)
	payload := EncodeHello(want)
	got, err := DecodeHello(payload, flags)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Errorf("hello mismatch: %+v want %+v", got, want)
	}
}

func TestHelloRejectsTruncatedPayload(t *testing.T) {
	if _, err := DecodeHello([]byte{1, 2, 3}, 0); err == nil {
		t.Fatal("expected error on truncated hello")
	}
}

func TestHelloRejectsNameLenOverrun(t *testing.T) {
	// file_size + name_len=999 but no name bytes → should fail.
	buf := make([]byte, 12)
	buf[8], buf[9], buf[10], buf[11] = 0, 0, 0x03, 0xe7 // 999
	if _, err := DecodeHello(buf, 0); err == nil {
		t.Fatal("expected error on name_len overrun")
	}
}

func TestRangeReqRoundtrip(t *testing.T) {
	want := RangeReqPayload{Offset: 1 << 30, Length: 1 << 20}
	got, err := DecodeRangeReq(EncodeRangeReq(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Errorf("range_req mismatch: %+v want %+v", got, want)
	}
}

func TestRangeReqRejectsWrongLength(t *testing.T) {
	if _, err := DecodeRangeReq(make([]byte, 15)); err == nil {
		t.Fatal("expected error on 15-byte payload")
	}
	if _, err := DecodeRangeReq(make([]byte, 17)); err == nil {
		t.Fatal("expected error on 17-byte payload")
	}
}

func TestRangeEndRoundtrip(t *testing.T) {
	want := RangeEndPayload{Status: 42}
	got, err := DecodeRangeEnd(EncodeRangeEnd(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Errorf("range_end mismatch: %+v want %+v", got, want)
	}
	if _, err := DecodeRangeEnd(make([]byte, 3)); err == nil {
		t.Fatal("expected error on short range_end payload")
	}
}

func TestSeekHintRoundtrip(t *testing.T) {
	want := SeekHintPayload{TimestampMs: 123_456}
	got, err := DecodeSeekHint(EncodeSeekHint(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Errorf("seek_hint mismatch: %+v want %+v", got, want)
	}
	if _, err := DecodeSeekHint(make([]byte, 7)); err == nil {
		t.Fatal("expected error on short seek_hint payload")
	}
}

func TestHelloFlagsHelper(t *testing.T) {
	if HelloFlags(false, false) != 0 {
		t.Error("expected 0 for both false")
	}
	if HelloFlags(true, false) != FlagTranscoding {
		t.Error("expected FlagTranscoding only")
	}
	if HelloFlags(false, true) != FlagSeekable {
		t.Error("expected FlagSeekable only")
	}
	if HelloFlags(true, true) != (FlagTranscoding | FlagSeekable) {
		t.Error("expected both flags")
	}
}

// Sanity check that MaxChunkPayload + HeaderSize fits inside MaxFrameSize so
// callers can rely on the chunk cap without their own bookkeeping.
func TestMaxChunkFitsInMaxFrame(t *testing.T) {
	if MaxChunkPayload+HeaderSize > MaxFrameSize {
		t.Fatalf("chunk %d + hdr %d > max frame %d", MaxChunkPayload, HeaderSize, MaxFrameSize)
	}
}
