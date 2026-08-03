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

// reserve claims est bytes BEFORE they are pulled, reporting whether the budget
// had room. Pair every true with exactly one settle.
//
// Reserving, not just charging afterwards, is what makes the ceiling hold under
// concurrency. Several readers draw on one pot (head + tail warm-up, read-ahead
// goroutines, successive volume readers), so a "check, fetch, then debit" order
// lets every one of them pass the check before the first debit lands and blow
// through the cap together — limiting is not reserving. Claiming up front means
// the Nth concurrent fetch sees the other N-1 already accounted for.
//
// est is the NZB's encoded Segment.Bytes, which runs 3-7% ABOVE the decoded
// bytes: over-reserving is the safe direction, and settle gives back the excess.
// A non-positive est carries no information, so it degrades to the old check —
// still bounded, just without the concurrency guarantee.
func (b *FetchBudget) reserve(est int64) bool {
	if b == nil {
		return true
	}
	if est <= 0 {
		return b.remaining.Load() > 0
	}
	if b.remaining.Add(-est) < 0 {
		b.remaining.Add(est) // put it back: this fetch is not happening
		return false
	}
	return true
}

// settle reconciles a reservation with what actually came off the wire: the
// difference goes back to the pot (or is taken from it, if the article was
// bigger than advertised) and Spent records the real bytes.
//
// actual is 0 when the fetch failed, which refunds the whole reservation — a
// failed Body call transferred nothing we can account for, matching how spend was
// recorded before reservations existed.
func (b *FetchBudget) settle(reserved, actual int64) {
	if b == nil {
		return
	}
	if actual > 0 {
		b.spent.Add(actual)
	}
	if reserved > 0 {
		b.remaining.Add(reserved - actual)
	} else {
		b.remaining.Add(-actual) // nothing was reserved: debit what we pulled
	}
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

// ApplyFetchBudget caps rd's NNTP spend at b, reporting whether the spend is now
// actually bounded. A false return means the caller must not speculatively read:
// either rd is not one of ours, or b is nil.
//
// A nil budget means UNLIMITED (NewFetchBudget returns nil for a non-positive
// size), so reporting success for it would turn every "refuse to read what I
// cannot bound" guard into its exact opposite — one mistyped ceiling and the
// warm-up drains the account instead of declining to run.
func ApplyFetchBudget(rd any, b *FetchBudget) bool {
	if b == nil {
		return false
	}
	br, ok := rd.(BudgetedReader)
	if !ok {
		return false
	}
	br.SetFetchBudget(b)
	return true
}
