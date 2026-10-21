// Package store provides a short-TTL memo cache over the vm and vl clients.
// It dedupes concurrent same-key requests within the TTL window and never
// caches an error.
package store

import (
	"sync"
	"time"
)

// TTL matches TTL_MS in lib/store.js: REFRESH_MS (20000ms) minus a 2000ms
// margin, floored at 1000ms by the JS's Math.max (moot at this value).
const TTL = 18 * time.Second

type entry struct {
	at   time.Time
	done chan struct{}
	val  any
	err  error
}

// Memo dedupes concurrent calls sharing a key within its TTL window.
type Memo struct {
	mu    sync.Mutex
	ttl   time.Duration
	cache map[string]*entry
}

// New returns a Memo caching results for ttl.
func New(ttl time.Duration) *Memo {
	return &Memo{ttl: ttl, cache: map[string]*entry{}}
}

// Do runs run and caches its result under key for the Memo's TTL. A call for
// the same key while a prior call is still within TTL waits on that call's
// result instead of running again, whether or not the prior call has
// finished yet. An error is never cached: it is deleted immediately so the
// next call retries.
func (m *Memo) Do(key string, run func() (any, error)) (any, error) {
	m.mu.Lock()
	if e, ok := m.cache[key]; ok && time.Since(e.at) < m.ttl {
		m.mu.Unlock()
		<-e.done
		if e.err != nil {
			return nil, e.err
		}
		return e.val, nil
	}
	e := &entry{at: time.Now(), done: make(chan struct{})}
	m.cache[key] = e
	m.mu.Unlock()

	e.val, e.err = run()
	if e.err != nil {
		m.mu.Lock()
		delete(m.cache, key)
		m.mu.Unlock()
	}
	close(e.done)
	return e.val, e.err
}
