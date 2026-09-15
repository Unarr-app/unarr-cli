package stream

import (
	"errors"
	"time"
)

// maxDeadArticles bounds the dead-article memo across all scopes. An entry is a
// message-id and an error, so 4096 is a few hundred KB at worst — and a release
// with thousands of dead articles is not being streamed anyway.
const maxDeadArticles = 4096

// deadMark is the memoised verdict for an article the server cannot deliver.
type deadMark struct {
	err   *ArticleUnavailableError
	until time.Time // zero: for the scope's lifetime (430/423); else a stall's expiry
}

// The dead-article memo.
//
// Before it, every request touching a dead article paid the whole failure again:
// each /usenet request builds a fresh Reader, and nothing remembered the 430. A
// dead article costs ONE BODY per source now: the first failure is published to
// the in-flight waiters (as any article-level error is) and memoised under the
// source's scope, so every later request and every read-ahead asking for it gets
// the verdict immediately, with no network. It lives and dies with the scope
// (purged on release, like the cached articles).

// deadLocked returns the memoised verdict for key, dropping an expired one.
func (c *ArticleCache) deadLocked(key cacheKey, now time.Time) (*ArticleUnavailableError, bool) {
	m, ok := c.dead[key]
	if !ok {
		return nil, false
	}
	if !m.until.IsZero() && now.After(m.until) {
		delete(c.dead, key)
		return nil, false
	}
	return m.err, true
}

// markDeadLocked memoises err for key when it is an article-level verdict.
func (c *ArticleCache) markDeadLocked(key cacheKey, err error, now time.Time) {
	var u *ArticleUnavailableError
	if !errors.As(err, &u) {
		return
	}
	if len(c.dead) >= maxDeadArticles {
		c.evictDeadLocked(now)
	}
	m := deadMark{err: u}
	if u.Stalled {
		m.until = now.Add(StalledArticleTTL)
	}
	c.dead[key] = m
}

// evictDeadLocked makes room in a full memo: expired verdicts first, else an
// arbitrary one (forgetting a verdict only costs one more BODY).
func (c *ArticleCache) evictDeadLocked(now time.Time) {
	for k, m := range c.dead {
		if !m.until.IsZero() && now.After(m.until) {
			delete(c.dead, k)
		}
	}
	for k := range c.dead {
		if len(c.dead) < maxDeadArticles {
			return
		}
		delete(c.dead, k)
	}
}

// purgeDeadLocked drops every verdict of scope s.
func (c *ArticleCache) purgeDeadLocked(s *CacheScope) {
	for k := range c.dead {
		if k.scope == s {
			delete(c.dead, k)
		}
	}
}

// settledFlight is an already-completed flight carrying a memoised failure, so a
// dead article flows through the same wait path as a live fetch.
func settledFlight(err error) *flight {
	fl := &flight{done: make(chan struct{}), err: err, finished: true}
	close(fl.done)
	return fl
}
