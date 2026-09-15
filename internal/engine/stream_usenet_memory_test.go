//go:build linux

package engine

// Memory and CPU profile of the /usenet streaming path at production article
// sizes: ~500 MiB of 716 KiB articles through the real handler, as a sequential
// read, a series of scrubs and a warm re-read, then a second stream to check that
// successive streams do not drift upward. Opt-in (UNARR_USENET_MEMPROFILE=1): it
// takes tens of seconds. The NNTP server runs in a child process so its own
// allocations and memory stay out of the numbers.
//
//	UNARR_USENET_MEMPROFILE=1 go test -run '^TestUsenetMemoryProfile$' -v ./internal/engine/

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nntptest"
	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

const (
	memProfileEnv       = "UNARR_USENET_MEMPROFILE"
	memProfileServerEnv = "UNARR_USENET_MEMPROFILE_SERVER"
	memPartSize         = 716 << 10
	memParts            = 720
	memConnections      = 10
	memChunk            = 256 << 10
	memName             = "profile.mkv"
)

func memFileSize() int64 { return int64(memPartSize) * memParts }

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// fillPattern writes the profile file's bytes at [off, off+len(dst)):
// pseudo-random, like compressed video, so yEnc escaping is realistic.
func fillPattern(dst []byte, off int64) {
	for i := 0; i < len(dst); {
		pos := off + int64(i)
		w := splitmix64(uint64(pos >> 3))
		for shift := uint(pos & 7); shift < 8 && i < len(dst); shift++ {
			dst[i] = byte(w >> (8 * shift))
			i++
		}
	}
}

func memMessageID(part int) string { return fmt.Sprintf("profile-p%d@fake.local", part) }

func memArticle(part int) []byte {
	data := make([]byte, memPartSize)
	start := int64(part-1) * memPartSize
	fillPattern(data, start)
	return yenc.Encode(memName, part, memParts, start+1, start+memPartSize, memFileSize(), data)
}

type memServerInfo struct {
	Host     string
	Port     int
	Segments []nzb.Segment
}

// TestUsenetMemoryProfileServer is the child process: it serves the generated
// articles and prints the segment table, until its stdin closes.
func TestUsenetMemoryProfileServer(t *testing.T) {
	if os.Getenv(memProfileServerEnv) == "" {
		t.Skip("child process of TestUsenetMemoryProfile")
	}
	s := nntptest.NewFakeServer(t)
	s.GenerateArticles(func(id string) ([]byte, bool) {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(id, "profile-p"), "@fake.local"))
		if err != nil || n < 1 || n > memParts {
			return nil, false
		}
		return memArticle(n), true
	})
	info := memServerInfo{Segments: make([]nzb.Segment, memParts)}
	info.Host, info.Port = s.Addr()
	for i := range info.Segments {
		info.Segments[i] = nzb.Segment{Bytes: int64(len(memArticle(i + 1))), Number: i + 1, MessageID: memMessageID(i + 1)}
	}
	if err := json.NewEncoder(os.Stdout).Encode(info); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func startMemProfileServer(t *testing.T) memServerInfo {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUsenetMemoryProfileServer$", "-test.timeout=0")
	cmd.Env = append(os.Environ(), memProfileServerEnv+"=1")
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	r := bufio.NewReader(stdout)
	line, err := r.ReadBytes('\n')
	var info memServerInfo
	if err == nil {
		err = json.Unmarshal(line, &info)
	}
	if err != nil {
		t.Fatalf("server info: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, r) }()
	return info
}

// memSample is one reading of the process's memory, in bytes.
type memSample struct{ heapInuse, heapSys, sys, rss uint64 }

func readRSS() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "VmRSS:" {
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

// memSampler tracks the peak of each memory figure between resets.
type memSampler struct {
	mu   sync.Mutex
	peak memSample
	stop chan struct{}
}

func startMemSampler(t *testing.T) *memSampler {
	s := &memSampler{stop: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-tick.C:
				s.sample()
			}
		}
	}()
	t.Cleanup(func() { close(s.stop); wg.Wait() })
	return s
}

func (s *memSampler) sample() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	rss := readRSS()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peak.heapInuse = max(s.peak.heapInuse, ms.HeapInuse)
	s.peak.heapSys = max(s.peak.heapSys, ms.HeapSys)
	s.peak.sys = max(s.peak.sys, ms.Sys)
	s.peak.rss = max(s.peak.rss, rss)
}

func (s *memSampler) reset() memSample {
	s.sample()
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peak
	s.peak = memSample{}
	return p
}

// profileFetcher counts BODY calls in front of the NNTP client.
type profileFetcher struct {
	inner *nntp.Client
	calls atomic.Int64
}

func (p *profileFetcher) Body(ctx context.Context, id string) ([]byte, error) {
	p.calls.Add(1)
	return p.inner.Body(ctx, id)
}

func (p *profileFetcher) BodyInto(ctx context.Context, id string, buf []byte) ([]byte, error) {
	p.calls.Add(1)
	return p.inner.BodyInto(ctx, id, buf)
}

// settle waits until no BODY has been issued for 300 ms.
func (p *profileFetcher) settle() int64 {
	last := p.calls.Load()
	for {
		time.Sleep(300 * time.Millisecond)
		if now := p.calls.Load(); now == last {
			return now
		} else {
			last = now
		}
	}
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// memPhase is what one workload cost.
type memPhase struct {
	name        string
	bytes       int64
	wall        time.Duration
	cpu, verify time.Duration
	mallocs     uint64
	allocBytes  uint64
	gcs         uint32
	bodies      int64
	peak        memSample
	endRSS      uint64
}

func (ph memPhase) log(t *testing.T) {
	gib := float64(ph.bytes) / (1 << 30)
	perGiB := func(v float64) float64 {
		if gib == 0 {
			return 0
		}
		return v / gib
	}
	mib := func(v uint64) float64 { return float64(v) / (1 << 20) }
	t.Logf("MEMPROFILE %-10s delivered=%.0fMiB wall=%v speed=%.0fMiB/s BODY=%d | peak HeapInuse=%.0fMiB HeapSys=%.0fMiB Sys=%.0fMiB RSS=%.0fMiB endRSS=%.0fMiB | allocs/GiB=%.0f allocMiB/GiB=%.0f GC=%d | CPU=%.2fs (verify %.2fs) CPU-excl-verify/GiB=%.2fs",
		ph.name, mib(uint64(ph.bytes)), ph.wall.Round(time.Millisecond), perGiB(mib(uint64(ph.bytes))/ph.wall.Seconds())*gib,
		ph.bodies, mib(ph.peak.heapInuse), mib(ph.peak.heapSys), mib(ph.peak.sys), mib(ph.peak.rss), mib(ph.endRSS),
		perGiB(float64(ph.mallocs)), perGiB(mib(ph.allocBytes)), ph.gcs,
		ph.cpu.Seconds(), ph.verify.Seconds(), perGiB((ph.cpu - ph.verify).Seconds()))
}

// measure runs work and records its cost. work returns the bytes delivered and
// the time spent verifying them.
func measure(name string, s *memSampler, pf *profileFetcher, work func() (int64, time.Duration)) memPhase {
	var before, after runtime.MemStats
	s.reset()
	runtime.ReadMemStats(&before)
	calls, cpu, start := pf.calls.Load(), cpuTime(), time.Now()
	n, verify := work()
	ph := memPhase{name: name, bytes: n, wall: time.Since(start), cpu: cpuTime() - cpu, verify: verify}
	runtime.ReadMemStats(&after)
	ph.bodies = pf.settle() - calls
	ph.mallocs = after.Mallocs - before.Mallocs
	ph.allocBytes = after.TotalAlloc - before.TotalAlloc
	ph.gcs = after.NumGC - before.NumGC
	ph.peak = s.reset()
	ph.endRSS = readRSS()
	return ph
}

// fetchRange GETs [lo, hi) (the whole file when hi <= 0), checks every byte and
// returns the bytes read and the time spent checking them.
func fetchRange(t *testing.T, url string, lo, hi int64) (int64, time.Duration) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if hi > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", lo, hi-1))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	buf, want := make([]byte, memChunk), make([]byte, memChunk)
	var n int64
	var verify time.Duration
	for {
		k, err := io.ReadFull(resp.Body, buf)
		if k > 0 {
			start := time.Now()
			fillPattern(want[:k], lo+n)
			if !bytes.Equal(buf[:k], want[:k]) {
				t.Fatalf("wrong bytes in [%d, %d)", lo+n, lo+n+int64(k))
			}
			verify += time.Since(start)
			n += int64(k)
		}
		if err != nil {
			break
		}
	}
	if hi > 0 && n != hi-lo {
		t.Fatalf("range [%d,%d) delivered %d bytes", lo, hi, n)
	}
	return n, verify
}

// scrubRanges are 16 player-style seeks of 4 MiB each, spread over the file out of
// order and off article boundaries.
func scrubRanges() [][2]int64 {
	const span = 4 << 20
	out := make([][2]int64, 0, 16)
	for k := int64(0); k < 16; k++ {
		lo := ((k*7)%16)*(memFileSize()-span)/16 + (k*123_457)%memPartSize
		out = append(out, [2]int64{lo, lo + span})
	}
	return out
}

func openMemStream(t *testing.T, pf *profileFetcher, info memServerInfo, id string) (*UsenetStreamHandle, string) {
	t.Helper()
	n := &nzb.NZB{Meta: map[string]string{}, Files: []nzb.File{{
		Subject:  fmt.Sprintf(`[profile] "%s" yEnc (1/%d)`, memName, memParts),
		Groups:   []string{"alt.binaries.test"},
		Segments: info.Segments,
	}}}
	ss := NewStreamServer(0, 1)
	handle, err := BuildUsenetStream(context.Background(), pf, n, ss, id)
	if err != nil {
		t.Fatalf("BuildUsenetStream: %v", err)
	}
	_, url := usenetFront(t, ss, id)
	return handle, url
}

func TestUsenetMemoryProfile(t *testing.T) {
	if os.Getenv(memProfileEnv) == "" {
		t.Skip("opt-in: set " + memProfileEnv + "=1")
	}
	info := startMemProfileServer(t)
	client := nntp.NewClient(nntp.Config{Host: info.Host, Port: info.Port, Username: "user", Password: "pass", MaxConnections: memConnections})
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	pf := &profileFetcher{inner: client}
	sampler := startMemSampler(t)
	t.Logf("MEMPROFILE idle-before RSS=%.0fMiB", float64(readRSS())/(1<<20))

	var phases []memPhase
	for round := 1; round <= 2; round++ {
		handle, url := openMemStream(t, pf, info, "memprofile-"+strconv.Itoa(round))
		phases = append(phases, measure(fmt.Sprintf("seq#%d", round), sampler, pf, func() (int64, time.Duration) {
			return fetchRange(t, url, 0, 0)
		}))
		if round == 1 {
			scrubs := scrubRanges()
			phases = append(phases, measure("scrub", sampler, pf, func() (int64, time.Duration) {
				var total int64
				var verify time.Duration
				for _, r := range scrubs {
					n, v := fetchRange(t, url, r[0], r[1])
					total, verify = total+n, verify+v
				}
				return total, verify
			}))
			// The head of the last scrub: its articles and their read-ahead were all
			// fetched by that scrub, so every BODY here is a cache miss.
			last := scrubs[len(scrubs)-1]
			phases = append(phases, measure("warm", sampler, pf, func() (int64, time.Duration) {
				return fetchRange(t, url, last[0], last[0]+(last[1]-last[0])/2)
			}))
		}
		handle.Close()
	}
	for _, ph := range phases {
		ph.log(t)
	}
	time.Sleep(10 * time.Second)
	t.Logf("MEMPROFILE idle-after RSS=%.0fMiB (10 s after the last stream closed)", float64(readRSS())/(1<<20))
}
