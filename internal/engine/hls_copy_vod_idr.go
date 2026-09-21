package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/winproc"
)

type copyVideoPacket struct {
	PTS   string `json:"pts_time"`
	Flags string `json:"flags"`
	Data  string `json:"data"`
}

type copyPacketBuffer struct{ bytes.Buffer }

func (b *copyPacketBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 32*1024*1024 {
		return 0, fmt.Errorf("copy keyframe probe exceeded output budget")
	}
	return b.Buffer.Write(p)
}

func firstCopyVideoPacket(ctx context.Context, ffprobe, path, interval string) (copyVideoPacket, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0", "-read_intervals", interval,
		"-show_entries", "stream=nal_length_size:packet=pts_time,flags,data", "-show_data", "-of", "json", path)
	winproc.HideWindow(cmd)
	var out copyPacketBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return copyVideoPacket{}, 0, err
	}
	var probe struct {
		Packets []copyVideoPacket `json:"packets"`
		Streams []struct {
			Length string `json:"nal_length_size"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		return copyVideoPacket{}, 0, err
	}
	if len(probe.Packets) != 1 || len(probe.Streams) != 1 {
		return copyVideoPacket{}, 0, fmt.Errorf("missing copy video packet")
	}
	n, _ := strconv.Atoi(probe.Streams[0].Length)
	return probe.Packets[0], n, nil
}

// Keyframe flags also include recovery-point I pictures in OPEN GOPs. They
// cannot be advertised as independently decodable HLS fragments. Inspect NAL
// types, not pict_type=I or packet.flags=K. AVCC sources and Annex-B TS supported.
func copyPacketHasIDR(dump string, lengthSize int) bool {
	data, ok := copyPacketBytes(dump)
	if !ok {
		return false
	}
	if lengthSize >= 1 && lengthSize <= 4 {
		return copyAVCCHasIDR(data, lengthSize)
	}
	for i := 0; i+3 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && data[i+3]&31 == 5 {
			return true
		}
	}
	return false
}

func copyPacketBytes(dump string) ([]byte, bool) {
	var data []byte
	for _, line := range strings.Split(dump, "\n") {
		_, rest, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		hexPart, _, _ := strings.Cut(rest, "  ")
		b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(hexPart), " ", ""))
		if err != nil {
			return nil, false
		}
		data = append(data, b...)
	}
	return data, true
}

func copyAVCCHasIDR(data []byte, lengthSize int) bool {
	for len(data) > lengthSize {
		n := 0
		for _, b := range data[:lengthSize] {
			n = n<<8 | int(b)
		}
		data = data[lengthSize:]
		if n <= 0 || n > len(data) {
			return false
		}
		if data[0]&31 == 5 {
			return true
		}
		data = data[n:]
	}
	return false
}

// A representative non-initial cut catches open-GOP encodes before exposing a
// VOD manifest. Every generated fragment is checked again before publication;
// a mixed/corrupt source cannot silently publish a non-IDR or misplaced cut.
func copySourceHasIDR(ctx context.Context, s *HLSSession, starts []float64) bool {
	at := 0.0
	if len(starts) > 2 {
		at = starts[1]
	}
	if s.cfg.StartSec > at {
		for _, p := range starts[1 : len(starts)-1] {
			if p > s.cfg.StartSec {
				break
			}
			at = p
		}
	}
	p, n, err := firstCopyVideoPacket(ctx, s.cfg.Transcode.FFprobePath, s.copySource(), fmt.Sprintf("%.6f%%+#1", at))
	return err == nil && copyPacketHasIDR(p.Data, n)
}
