package srcproxy

import (
	"container/list"
	"os"
	"sync"
)

// blockStore is a fixed-size, slot-addressed block cache backed by ONE file.
// Disk (not RAM) because the agent also runs on small NAS boxes; a slot file
// rather than a sparse one because hole-punching is not portable to Windows.
// Pinned blocks (container header / seek index) are never evicted; the rest is
// LRU. Every method holds mu across the file I/O so an evicted slot can never be
// rewritten while another goroutine is still reading it.
type blockStore struct {
	mu     sync.Mutex
	f      *os.File
	slots  int
	next   int // next never-used slot
	pinned func(idx int64) bool
	index  map[int64]*blockEntry
	lru    *list.List // front = most recent; unpinned entries only
}

type blockEntry struct {
	idx  int64
	slot int
	n    int
	el   *list.Element // nil when pinned
}

func newBlockStore(path string, slots int, pinned func(int64) bool) (*blockStore, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: path is the session's own tmp dir.
	if err != nil {
		return nil, err
	}
	return &blockStore{f: f, slots: slots, pinned: pinned, index: make(map[int64]*blockEntry), lru: list.New()}, nil
}

// get copies block idx into buf and reports its length. ok=false on a miss or a
// read error (the caller then refetches; a bad cache must never fail playback).
func (b *blockStore) get(idx int64, buf []byte) (n int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.index[idx]
	if e == nil || b.f == nil {
		return 0, false
	}
	if _, err := b.f.ReadAt(buf[:e.n], int64(e.slot)*blockSize); err != nil {
		b.drop(e)
		return 0, false
	}
	if e.el != nil {
		b.lru.MoveToFront(e.el)
	}
	return e.n, true
}

// put stores data as block idx. Best-effort: a full cache of pinned blocks or a
// write error simply leaves the block uncached.
func (b *blockStore) put(idx int64, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.f == nil || b.index[idx] != nil {
		return
	}
	slot := b.alloc()
	if slot < 0 {
		return
	}
	if _, err := b.f.WriteAt(data, int64(slot)*blockSize); err != nil {
		return // slot is lost for this session; harmless
	}
	e := &blockEntry{idx: idx, slot: slot, n: len(data)}
	if !b.pinned(idx) {
		e.el = b.lru.PushFront(e)
	}
	b.index[idx] = e
}

func (b *blockStore) alloc() int {
	if b.next < b.slots {
		b.next++
		return b.next - 1
	}
	back := b.lru.Back()
	if back == nil {
		return -1
	}
	e := back.Value.(*blockEntry) //nolint:errcheck,forcetypeassert // Only *blockEntry is ever pushed.
	slot := e.slot
	b.drop(e)
	return slot
}

func (b *blockStore) drop(e *blockEntry) {
	if e.el != nil {
		b.lru.Remove(e.el)
	}
	delete(b.index, e.idx)
}

func (b *blockStore) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.f == nil {
		return nil
	}
	name := b.f.Name()
	err := b.f.Close()
	b.f = nil
	_ = os.Remove(name)
	return err
}
