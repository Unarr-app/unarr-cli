package stream

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/usenet/nntp"
	"github.com/Unarr-app/unarr-cli/internal/usenet/yenc"
)

// Dead-article tuning. Package vars (like ReadaheadBytes) so a caller/test can
// retune them without a rebuild.
var (
	// ArticleStallTimeout bounds how long one BODY waits for the server's status
	// line on the streaming path (nntp.WithStallTimeout). A provider that stalls
	// instead of answering 430 used to hold a read for the 60 s command deadline,
	// doubled by the reconnect retry and tripled by the reader's attempts — ~6 min
	// for one dead article, with a player waiting. 15 s is far above a healthy
	// status-line round trip and well below what a player tolerates in total.
	ArticleStallTimeout = 15 * time.Second
	// StalledArticleTTL is how long a stalled article stays memoised as
	// unavailable. Unlike a 430 a stall may be the server's transient trouble, so
	// the verdict expires; within it every request fails fast instead of paying
	// the stall bound again.
	StalledArticleTTL = 30 * time.Second
)

// ErrArticleUnavailable matches (errors.Is) every ArticleUnavailableError.
var ErrArticleUnavailable = errors.New("usenet: article unavailable")

// ArticleUnavailableError is a read failure caused by one article the server
// cannot deliver: it answered "no such article" (430/423), or it stalled past
// ArticleStallTimeout. It is final for the source (see the dead-article memo), so
// a caller can fail a request cleanly instead of retrying into the same hole.
type ArticleUnavailableError struct {
	MessageID string
	Stalled   bool // the server stalled (memoised for StalledArticleTTL) rather than answering 430
	Err       error
}

func (e *ArticleUnavailableError) Error() string {
	if e.Stalled {
		return fmt.Sprintf("usenet: article %s stalled: %v", e.MessageID, e.Err)
	}
	return fmt.Sprintf("usenet: article %s unavailable: %v", e.MessageID, e.Err)
}

func (e *ArticleUnavailableError) Unwrap() error { return e.Err }

// Is lets errors.Is(err, ErrArticleUnavailable) match.
func (e *ArticleUnavailableError) Is(target error) bool { return target == ErrArticleUnavailable }

// asUnavailable classifies a Body error: non-nil when it is final for the article.
func asUnavailable(messageID string, err error) *ArticleUnavailableError {
	switch {
	case nntp.IsArticleMissing(err):
		return &ArticleUnavailableError{MessageID: messageID, Err: err}
	case nntp.IsStalled(err):
		return &ArticleUnavailableError{MessageID: messageID, Stalled: true, Err: err}
	}
	return nil
}

// fetchDecodeRetry fetches and yEnc-decodes one article, retrying transient
// failures (a dropped connection, a corrupt body) up to maxAttempts with a
// bounded backoff. An article the server says does not exist (430/423), or that
// stalls past the stall bound, is NOT retried: it would get the same answer, and
// the retries were what turned one dead article into seconds and a dozen BODY
// commands per request. Every failure is logged; the final error is wrapped so
// the caller can degrade cleanly rather than hang.
func (r *Reader) fetchDecodeRetry(messageID string, estBytes int64) (*yenc.Part, error) {
	var lastErr error
	for attempt := 0; attempt < r.maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(r.retryBackoff):
			case <-r.ctx.Done():
				return nil, r.ctx.Err()
			}
		}
		// Cost ceiling BEFORE the wire, claimed rather than merely checked: a
		// speculative reader (cold-buffer warm-up, no player attached) stops pulling
		// once its byte budget is spoken for, however much wall clock is left.
		// Usenet is billed by volume, so this is the bound that protects the
		// account — and it is per ATTEMPT, because a retry re-transfers the article.
		if !r.budget.reserve(estBytes) {
			return nil, ErrFetchBudgetExhausted
		}
		raw, err := r.fetcher.Body(nntp.WithStallTimeout(r.ctx, r.stallTimeout), messageID)
		// Reconcile against what came off the wire, whether or not it decodes — a
		// corrupt body was still transferred and still billed. A failed Body
		// transferred nothing we can account for and refunds the reservation.
		r.budget.settle(estBytes, int64(len(raw)))
		if err == nil {
			var part *yenc.Part
			if part, err = yenc.DecodeBytes(raw); err == nil {
				return part, nil
			}
			err = fmt.Errorf("decode: %w", err)
		} else if u := asUnavailable(messageID, err); u != nil {
			log.Printf("[usenet-stream] %v - not retrying", u)
			return nil, u
		}
		lastErr = err
		log.Printf("[usenet-stream] article %s attempt %d/%d failed: %v",
			messageID, attempt+1, r.maxAttempts, err)
	}
	return nil, fmt.Errorf("usenet reader: article %s failed after %d attempts: %w",
		messageID, r.maxAttempts, lastErr)
}
