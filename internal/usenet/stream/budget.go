package stream

import (
	"errors"
	"sync/atomic"
)

// ErrFetchBudgetExhausted is returned by a Reader whose FetchBudget has run out.
// It is a NORMAL, expected outcome for a bounded warm-up (we asked for a few MB
// and got them), not a fault: callers treat it like a short read / clean EOF and
// carry on. Test with errors.Is.
var ErrFetchBudgetExhausted = errors.New("usenet reader: fetch budget exhausted")

// FetchBudget is a hard ceiling on the NNTP bytes a set of Readers may pull,
// shared by pointer so several readers (the head + tail warm-up goroutines, or the
// successive per-volume readers of a RAR stream) draw on ONE pot.
//
// Why a byte budget and not just a timeout: Usenet is billed by VOLUME, and the
// upstream sustains ~55 MB/s per pool. A wall-clock bound of even a few seconds
// therefore permits hundreds of megabytes — a time bound is not a cost bound. Any
// speculative fetch with no player attached (the cold-buffer warm-up) must be
// capped in the unit we actually pay in.
//
// Zero value / nil pointer means "unbounded": live playback, which is driven by a
// real consumer, is deliberately not budgeted here.
type FetchBudget struct {
	remaining atomic.Int64
	spent     atomic.Int64
}

// NewFetchBudget returns a budget of maxBytes. A non-positive maxBytes yields nil
// (unbounded), so callers can wire a budget unconditionally and switch it off with
// a 0.
func NewFetchBudget(maxBytes int64) *FetchBudget {
	if maxBytes <= 0 {
		return nil
	}
	b := &FetchBudget{}
	b.remaining.Store(maxBytes)
	return b
}

// charge debits n bytes and reports whether the budget still had room BEFORE the
// debit. It always records the spend, so Spent stays truthful about what was
// actually pulled even on the article that crossed the line. A nil budget is
// unbounded and always allows.
func (b *FetchBudget) charge(n int64) bool {
	if b == nil {
		return true
	}
	b.spent.Add(n)
	return b.remaining.Add(-n) > -n // true iff remaining was > 0 before the debit
}

// exhausted reports whether the budget is used up. A nil budget never is.
func (b *FetchBudget) exhausted() bool {
	return b != nil && b.remaining.Load() <= 0
}

// Spent returns the bytes charged against this budget so far — the number to log
// and to reason about cost with.
func (b *FetchBudget) Spent() int64 {
	if b == nil {
		return 0
	}
	return b.spent.Load()
}

// BudgetedReader is implemented by every reader the Usenet stream path hands out
// (Reader for a direct posting, rarVideoReader for a STORE archive), so a caller
// that only holds an io.ReadSeekCloser can still cap what it costs.
type BudgetedReader interface {
	SetFetchBudget(b *FetchBudget)
}

// ApplyFetchBudget caps rd's NNTP spend at b, reporting whether rd supported it.
// A false return means the reader is not one of ours — the caller should treat
// that as "cannot bound this, do not speculatively read".
func ApplyFetchBudget(rd any, b *FetchBudget) bool {
	br, ok := rd.(BudgetedReader)
	if !ok {
		return false
	}
	br.SetFetchBudget(b)
	return true
}
