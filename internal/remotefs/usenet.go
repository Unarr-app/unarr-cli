package remotefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nzb"
	"github.com/Unarr-app/unarr-cli/internal/usenet/stream"
)

// NZBSource watches NZB manifests, never downloaded media. Plans are bounded
// separately from the process-wide byte-bounded article cache.
type NZBSource struct {
	Directory    string
	Fetcher      stream.ArticleFetcher
	CloseFetcher func() error
	mu           sync.Mutex
	plans        map[string]*cachedPlan
	clock        uint64
	flights      singleflight.Group
	closed       bool
}
type cachedPlan struct {
	plan *stream.StreamPlan
	used uint64
}

func (*NZBSource) Name() string       { return "usenet" }
func (s *NZBSource) Identity() string { return digest("nzb:" + s.Directory) }
func (s *NZBSource) Close() error {
	s.mu.Lock()
	s.closed = true
	for _, p := range s.plans {
		p.plan.Close()
	}
	s.plans = nil
	s.mu.Unlock()
	if s.CloseFetcher != nil {
		return s.CloseFetcher()
	}
	return nil
}

func readManifest(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const max = 32 << 20
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if len(data) > max {
		return nil, errors.New("NZB exceeds 32 MiB")
	}
	return data, err
}

func (s *NZBSource) List(ctx context.Context, previous []Record) ([]Record, error) {
	files, err := os.ReadDir(s.Directory)
	if err != nil {
		return nil, err
	}
	old := previousReleases(previous)
	var records []Record
	for _, file := range files {
		if file.IsDir() || file.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(file.Name()), ".nzb") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := digest(file.Name())[:16]
		fresh, err := s.indexFile(ctx, file, old[id])
		if err == nil {
			records = append(records, fresh...)
			continue
		}
		// Independent direct files can survive a missing sidecar or episode.
		// Retain known entries and retry partial manifests on the next refresh.
		log.Printf("[mount] NZB %q indexing incomplete: %v", file.Name(), err)
		records = append(records, partialManifest(fresh, old[id])...)
	}
	return records, nil
}

func partialManifest(fresh, previous []Record) []Record {
	seen := make(map[string]bool, len(fresh))
	out := make([]Record, 0, len(fresh)+len(previous))
	for _, group := range [][]Record{fresh, previous} {
		for _, r := range group {
			if seen[r.Path] {
				continue
			}
			seen[r.Path] = true
			r.ManifestStamp = "" // Do not cache a partially successful probe.
			out = append(out, r)
		}
	}
	return out
}

func (s *NZBSource) indexFile(ctx context.Context, file os.DirEntry, previous []Record) ([]Record, error) {
	info, err := file.Info()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("NZB must be a regular file")
	}
	stamp := fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	if len(previous) > 0 && previous[0].ManifestStamp == stamp {
		return previous, nil
	}
	data, err := readManifest(filepath.Join(s.Directory, file.Name()))
	if err != nil {
		return nil, err
	}
	fingerprint := digest(string(data))
	if len(previous) > 0 && previous[0].ManifestStamp != "" && previous[0].Fingerprint == fingerprint {
		out := append([]Record(nil), previous...)
		for i := range out {
			out[i].ManifestStamp = stamp
		}
		return out, nil
	}
	parsed, err := nzb.ParseBytes(data)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return s.indexManifest(rctx, parsed, manifestInfo{file.Name(), digest(file.Name())[:16], fingerprint, stamp, info.ModTime()}, previous)
}

type manifestInfo struct {
	name, id, fingerprint, stamp string
	modified                     time.Time
}

func (s *NZBSource) indexManifest(ctx context.Context, n *nzb.NZB, info manifestInfo, previous []Record) ([]Record, error) {
	var records []Record
	var failures []error
	known := knownManifestFiles(previous, info.fingerprint)
	title := strings.TrimSuffix(info.name, filepath.Ext(info.name))
	paths := memberPaths(n.Files, title, info.id)
	indices := []int{-1}
	if !n.HasRars() {
		indices = nil
		for i := range n.Files {
			indices = append(indices, i)
		}
	}
	for _, idx := range indices {
		if err := ctx.Err(); err != nil {
			return records, err
		}
		if record, ok := known[idx]; ok {
			record.ManifestStamp = info.stamp
			record.Modified = info.modified
			records = append(records, record)
			continue
		}
		plan := s.makePlan(ctx, n, idx)
		if !plan.Streamable() {
			plan.Close()
			failures = append(failures, fmt.Errorf("%w: %s", stream.ErrNotStreamable, plan.Reason))
			continue
		}
		memberPath := releasePath(title, info.id, plan.VideoName)
		if idx >= 0 {
			memberPath = paths[idx]
		}
		records = append(records, Record{Entry: Entry{
			Path: memberPath, Key: info.fingerprint + ":" + strconv.Itoa(idx), Size: plan.VideoSize, Modified: info.modified,
		}, ID: info.id, Manifest: info.name, Fingerprint: info.fingerprint, ManifestStamp: info.stamp, FileIndex: idx})
		plan.Close()
	}
	if len(records) == 0 {
		failures = append(failures, errors.New("NZB has no supported files"))
	}
	return records, errors.Join(failures...)
}

func knownManifestFiles(previous []Record, fingerprint string) map[int]Record {
	known := make(map[int]Record, len(previous))
	for _, record := range previous {
		if record.Fingerprint == fingerprint {
			known[record.FileIndex] = record
		}
	}
	return known
}

func (s *NZBSource) makePlan(ctx context.Context, n *nzb.NZB, idx int) *stream.StreamPlan {
	if n.Password == "" && idx >= 0 && idx < len(n.Files) {
		return stream.PlanFile(ctx, s.Fetcher, n.Files[idx])
	}
	return stream.StreamPlanFromNZB(ctx, s.Fetcher, n)
}

func (s *NZBSource) Open(ctx context.Context, rec Record) (io.ReadSeekCloser, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, os.ErrClosed
		}
		s.clock++
		if cached := s.plans[rec.Key]; cached != nil {
			cached.used = s.clock
			r := cached.plan.Open(ctx)
			s.mu.Unlock()
			return r, nil
		}
		s.mu.Unlock()
		// Only callers of this file wait for its header probe. Cached reads
		// and other files are never serialized behind remote I/O.
		result := s.flights.DoChan(rec.Key, func() (any, error) { return nil, s.loadPlan(ctx, rec) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-result:
			if r.Err != nil && !errors.Is(r.Err, context.Canceled) {
				return nil, r.Err
			}
		}
	}
}

func (s *NZBSource) loadPlan(ctx context.Context, rec Record) error {
	if filepath.Base(rec.Manifest) != rec.Manifest {
		return errors.New("invalid NZB manifest path")
	}
	data, err := readManifest(filepath.Join(s.Directory, rec.Manifest))
	if err != nil {
		return err
	}
	if digest(string(data)) != rec.Fingerprint {
		return errors.New("NZB changed; wait for catalog refresh")
	}
	n, err := nzb.ParseBytes(data)
	if err != nil {
		return err
	}
	plan := s.makePlan(ctx, n, rec.FileIndex)
	if !plan.Streamable() || plan.VideoSize != rec.Size {
		plan.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("NZB source unavailable or size changed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		plan.Close()
		return os.ErrClosed
	}
	if s.plans[rec.Key] != nil {
		plan.Close()
		return nil
	}
	if s.plans == nil {
		s.plans = make(map[string]*cachedPlan)
	}
	s.evictPlan()
	s.plans[rec.Key] = &cachedPlan{plan: plan, used: s.clock}
	return nil
}
func (s *NZBSource) evictPlan() {
	if len(s.plans) < 16 {
		return
	}
	var oldest string
	used := ^uint64(0)
	for k, p := range s.plans {
		if p.used < used {
			oldest, used = k, p.used
		}
	}
	s.plans[oldest].plan.Close()
	delete(s.plans, oldest)
}
