package cmd

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These barriers hold actual synthetic requests, never daemon code or timers.
// Every wait observes request cancellation so shutdown cannot hang the fixture.
type nativeStartupGates struct {
	mirrorEntered chan context.Context
	mirrorRelease chan struct{}
	registerOpen  chan struct{}
	accessOpen    chan struct{}
	mirrorCount   atomic.Int32
	accessPaused  atomic.Bool
	mirrorOnce    sync.Once
	registerOnce  sync.Once
	accessOnce    sync.Once
}

func newNativeStartupGates() *nativeStartupGates {
	return &nativeStartupGates{
		mirrorEntered: make(chan context.Context, 1), mirrorRelease: make(chan struct{}),
		registerOpen: make(chan struct{}), accessOpen: make(chan struct{}),
	}
}

func (g *nativeStartupGates) waitMirror(r *http.Request) bool {
	if g.mirrorCount.Add(1) != 1 {
		return true
	}
	select {
	case g.mirrorEntered <- r.Context():
	case <-r.Context().Done():
		return false
	}
	return nativeWaitRequestGate(r, g.mirrorRelease)
}

func (g *nativeStartupGates) waitRegister(r *http.Request) bool {
	return nativeWaitRequestGate(r, g.registerOpen)
}

func (g *nativeStartupGates) waitAccess(r *http.Request) bool {
	if !g.accessPaused.Load() {
		return true
	}
	return nativeWaitRequestGate(r, g.accessOpen)
}

func nativeWaitRequestGate(r *http.Request, gate <-chan struct{}) bool {
	select {
	case <-r.Context().Done():
		return false
	case <-gate:
		return r.Context().Err() == nil
	}
}

func (g *nativeStartupGates) releaseMirror() {
	g.mirrorOnce.Do(func() { close(g.mirrorRelease) })
}

func (g *nativeStartupGates) releaseAll() {
	g.releaseMirror()
	g.registerOnce.Do(func() { close(g.registerOpen) })
	g.accessOnce.Do(func() { close(g.accessOpen) })
}

// Cancellation is essential: a retained fake request must never prevent the
// real daemon from releasing its lock and draining its mounted child.
func TestNativeStartupGatesRequestCancellation(t *testing.T) {
	for _, phase := range []string{"mirrors", "register", "access"} {
		t.Run(phase, func(t *testing.T) {
			gates := newNativeStartupGates()
			defer gates.releaseAll()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/", nil)
			if err != nil {
				t.Fatal(err)
			}
			gates.accessPaused.Store(true)
			done := make(chan bool, 1)
			go func() {
				switch phase {
				case "mirrors":
					done <- gates.waitMirror(request)
				case "register":
					done <- gates.waitRegister(request)
				default:
					done <- gates.waitAccess(request)
				}
			}()
			cancel()
			select {
			case allowed := <-done:
				if allowed {
					t.Fatal("cancelled request passed a retained startup barrier")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled startup request did not release the fixture")
			}
			gates.releaseAll()
			request = request.WithContext(context.Background())
			if !gates.waitRegister(request) || !gates.waitAccess(request) {
				t.Fatal("released live request remained blocked")
			}
		})
	}
}
