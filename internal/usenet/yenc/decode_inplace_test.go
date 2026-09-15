package yenc

import (
	"bytes"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// DecodeInPlace must agree with Decode on every input: same part, same data, and
// an error exactly when Decode errors.
func TestDecodeInPlaceMatchesDecode(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	payload := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.UintN(256))
		}
		return b
	}
	multi := Encode("movie.mkv", 3, 9, 1_001, 1_000+70_000, 900_000, payload(70_000))
	single := Encode("clip.mkv", 1, 1, 1, 5_000, 5_000, payload(5_000))
	corrupt := bytes.Clone(multi)
	corrupt[len(corrupt)/2] ^= 0x55

	cases := map[string][]byte{
		"multipart":                multi,
		"single part":              single,
		"LF line endings":          []byte(strings.ReplaceAll(string(multi), "\r\n", "\n")),
		"garbage before ybegin":    append([]byte("hello\r\n\r\nworld\r\n"), multi...),
		"no trailing newline":      bytes.TrimSuffix(multi, []byte("\r\n")),
		"truncated before yend":    multi[:len(multi)/3],
		"crc mismatch":             corrupt,
		"no header":                []byte("just some text\r\nmore\r\n"),
		"empty":                    nil,
		"data line right after":    []byte("=ybegin line=128 size=3 name=x\r\n\x8b\x8c\x8d\r\n=yend size=3\r\n"),
		"escape at end of line":    []byte("=ybegin line=128 size=3 name=x\r\n\x8b=\r\n=}\x8c\r\n=yend size=3\r\n"),
		"lone CR line":             []byte("=ybegin line=128 size=3 name=x\r\n=ypart begin=1 end=2\r\n\r\n\x8b\x8c\r"),
		"ybegin without name/size": []byte("=ybegin part=1\r\nabc\r\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			want, wantErr := Decode(bytes.NewReader(in))
			got, gotErr := DecodeInPlace(bytes.Clone(in))
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("error mismatch: Decode %v, DecodeInPlace %v", wantErr, gotErr)
			}
			if wantErr != nil {
				return
			}
			if !bytes.Equal(got.Data, want.Data) {
				t.Fatalf("data mismatch: %d vs %d bytes", len(got.Data), len(want.Data))
			}
			got.Data, want.Data = nil, nil
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("part = %+v, want %+v", *got, *want)
			}
		})
	}
}

// The decoded part reuses the input's backing array: no second article-sized
// allocation.
func TestDecodeInPlaceReusesBuffer(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 10_000)
	buf := Encode("x.bin", 1, 2, 1, int64(len(data)), int64(2*len(data)), data)
	part, err := DecodeInPlace(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(part.Data, data) {
		t.Fatal("wrong data")
	}
	if &part.Data[0] != &buf[0] || cap(part.Data) != cap(buf) {
		t.Fatal("DecodeInPlace did not decode into the input buffer")
	}
	if allocs := testing.AllocsPerRun(20, func() {
		b := Encode("x.bin", 1, 2, 1, int64(len(data)), int64(2*len(data)), data)
		_, _ = DecodeInPlace(b)
	}) - testing.AllocsPerRun(20, func() {
		_ = Encode("x.bin", 1, 2, 1, int64(len(data)), int64(2*len(data)), data)
	}); allocs > 8 {
		t.Fatalf("DecodeInPlace allocated %.0f times, want only the part and its header strings", allocs)
	}
}
