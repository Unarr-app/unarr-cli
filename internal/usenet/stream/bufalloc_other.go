//go:build !unix || race

package stream

// allocOffHeap has no off-heap mapping here: large buffers stay on the Go heap,
// and are only recycled. Race builds take this path too, because the race
// detector does not watch memory outside the Go heap.
func allocOffHeap(n int) []byte { return make([]byte, 0, n) }

// freeOffHeap leaves a dropped buffer to the collector.
func freeOffHeap([]byte) {}
