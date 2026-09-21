// Package engine — COPY-VOD segment production pipeline.
//
// Generation is owned by the SESSION, never by the HTTP request that first asked
// for a segment. hls.js abandons a fragment request after ~10 s without a first
// byte; when generation hung off r.Context() that abort killed ffmpeg, threw the
// partial work away, and the retry started from zero — so on a slow remote link
// a segment that needed 11 s could never be produced at all (endless spinner).
// Now an abandoned request just stops waiting, and its retry joins the same run.
//
// A sequential prefetcher keeps a few segments ahead of the playhead so the
// player is served from disk instead of paying a cold ffmpeg spawn per fragment.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
)

// copyVODLookahead is how many segments past the last requested one are kept
// generated. ~3×6 s rides out a CDN hiccup without hoarding disk or bandwidth
// for a viewer who may seek away.
const copyVODLookahead = 3

// copyGenCall is one single-flight segment generation.
type copyGenCall struct {
	done   chan struct{}
	err    error // valid once done is closed
	cancel context.CancelFunc
	// prefetch is true while only the prefetcher wants this segment; a viewer
	// request joining the call clears it. Guarded by HLSSession.copyGenMu.
	prefetch bool
}

// ensureCopySegment blocks until segment idx is on disk, the request is
// abandoned, or the session closes. Abandoning the wait does NOT stop the run.
func (s *HLSSession) ensureCopySegment(ctx context.Context, idx int) error {
	if idx < 0 || idx >= s.segmentCount {
		return fmt.Errorf("hls: segment out of range")
	}
	if !s.copyLazy {
		return s.waitForSegment(ctx, idx)
	}
	if s.copySegmentReady(idx) {
		s.noteCopyPlayhead(idx)
		s.subWin.offer(idx) // a segment reused from the HLS cache was never generated here
		return nil
	}
	call := s.startCopyGen(idx, false)
	if call == nil {
		return context.Canceled
	}
	s.noteCopyPlayhead(idx)
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *HLSSession) copySegmentReady(idx int) bool {
	fi, err := os.Stat(s.copySegPath(idx))
	return err == nil && fi.Size() > 0
}

// startCopyGen returns the in-flight generation of idx, starting one if needed.
// nil means the session is closed.
func (s *HLSSession) startCopyGen(idx int, prefetch bool) *copyGenCall {
	s.copyGenMu.Lock()
	defer s.copyGenMu.Unlock()
	if call := s.copyGen[idx]; call != nil {
		if !prefetch {
			call.prefetch = false
		}
		return call
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.copyWG.Add(1)
	s.mu.Unlock()

	// The call owns cancel: runCopyGen defers it, and a seek may fire it earlier.
	ctx, cancel := context.WithCancel(s.copyCtx) //nolint:gosec // G118: released by runCopyGen's deferred call.cancel().
	call := &copyGenCall{done: make(chan struct{}), cancel: cancel, prefetch: prefetch}
	if s.copyGen == nil {
		s.copyGen = make(map[int]*copyGenCall)
	}
	s.copyGen[idx] = call
	go s.runCopyGen(ctx, idx, call)
	return call
}

func (s *HLSSession) runCopyGen(ctx context.Context, idx int, call *copyGenCall) {
	defer s.copyWG.Done()
	defer call.cancel()
	err := s.produceCopySegment(ctx, idx)
	if err == nil {
		s.subWin.offer(idx) // its bytes are in the proxy cache right now
	}
	// Forget the call BEFORE waking waiters, so a retry after a failure starts a
	// fresh run instead of joining the dead one.
	s.copyGenMu.Lock()
	delete(s.copyGen, idx)
	s.copyGenMu.Unlock()
	call.err = err
	close(call.done)
}

// produceCopySegment bounds parallel ffmpeg processes, then generates idx.
func (s *HLSSession) produceCopySegment(ctx context.Context, idx int) error {
	if s.copySlots != nil {
		select {
		case s.copySlots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		defer func() { <-s.copySlots }()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.copySegmentReady(idx) {
		return nil
	}
	if s.copyGenerate != nil {
		return s.copyGenerate(ctx, idx)
	}
	return s.generateCopySegment(ctx, idx)
}

// noteCopyPlayhead records the viewer's position and wakes the prefetcher. A
// jump outside the prefetch window is a seek: in-flight prefetches for the old
// position are cancelled so they stop competing with the segment now wanted.
func (s *HLSSession) noteCopyPlayhead(idx int) {
	s.copyGenMu.Lock()
	prev := s.copyHead
	s.copyHead = idx
	seeked := idx < prev || idx > prev+copyVODLookahead+1
	if seeked {
		for i, call := range s.copyGen {
			if call.prefetch && (i <= idx || i > idx+copyVODLookahead) {
				call.cancel()
			}
		}
	}
	s.copyGenMu.Unlock()
	if seeked {
		s.subWin.seek(idx) // subtitles follow the viewer too
	}
	select {
	case s.copyWake <- struct{}{}:
	default:
	}
}

// runCopyPrefetch is the session's prefetch loop. Caller did copyWG.Add(1).
func (s *HLSSession) runCopyPrefetch() {
	defer s.copyWG.Done()
	for {
		select {
		case <-s.copyCtx.Done():
			return
		case <-s.copyWake:
		}
		for s.prefetchNextCopySegment() {
		}
	}
}

// prefetchNextCopySegment advances the pipeline by one step and reports whether
// there may be more to do. Strictly sequential: on a bandwidth-bound link two
// parallel fetches finish no sooner than two serial ones, they only delay the
// segment the viewer is actually waiting for.
func (s *HLSSession) prefetchNextCopySegment() bool {
	s.copyGenMu.Lock()
	head := s.copyHead
	demand := s.copyGen[head]
	s.copyGenMu.Unlock()
	if demand != nil {
		return s.awaitCopyGen(demand)
	}
	next := s.nextMissingCopySegment(head)
	if next < 0 {
		return false
	}
	call := s.startCopyGen(next, true)
	if call == nil || !s.awaitCopyGen(call) {
		return false
	}
	if call.err != nil {
		if !errors.Is(call.err, context.Canceled) {
			log.Printf("[hls %s] copy-vod prefetch seg-%d failed: %v", shortHLSID(s.cfg.SessionID), next, call.err)
		}
		return false // wait for the next viewer request before trying again
	}
	return true
}

func (s *HLSSession) awaitCopyGen(call *copyGenCall) bool {
	select {
	case <-call.done:
		return true
	case <-s.copyCtx.Done():
		return false
	}
}

// nextMissingCopySegment is the first ungenerated index in the prefetch window
// after head, or -1 when the window is full.
func (s *HLSSession) nextMissingCopySegment(head int) int {
	for i := head + 1; i <= head+copyVODLookahead && i < s.segmentCount; i++ {
		if !s.copySegmentReady(i) {
			return i
		}
	}
	return -1
}
