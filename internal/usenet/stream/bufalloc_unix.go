//go:build unix && !race

package stream

import (
	"log"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// mmapFailed is set once a mapping was refused, so the failure is logged once.
var mmapFailed atomic.Bool

// allocOffHeap maps n bytes (rounded up to whole pages) outside the Go heap, or
// returns nil if the kernel refuses.
func allocOffHeap(n int) []byte {
	page := unix.Getpagesize()
	b, err := unix.Mmap(-1, 0, (n+page-1)/page*page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		if mmapFailed.CompareAndSwap(false, true) {
			log.Printf("[usenet-stream] off-heap article buffers unavailable, using the heap: %v", err)
		}
		return nil
	}
	return b[:0]
}

// freeOffHeap unmaps a buffer allocOffHeap returned, handing its pages straight
// back to the OS.
func freeOffHeap(b []byte) {
	if err := unix.Munmap(b[:cap(b)]); err != nil {
		log.Printf("[usenet-stream] unmap article buffer: %v", err)
	}
}
