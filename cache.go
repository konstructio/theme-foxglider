package main

import (
	"sync"
	"time"
)

const errTTL = 5 * time.Second

type entry struct {
	ready chan struct{}
	val   any
	err   error
	exp   time.Time
}

type cache struct {
	mu sync.Mutex
	m  map[string]*entry
}

func newCache() *cache { return &cache{m: map[string]*entry{}} }

// drop invalidates a completed entry so the next do() refetches — the
// post-mutation refresh (e.g. a branch was just deleted). In-flight entries
// are left alone; they expire on their own TTL.
func (c *cache) drop(key string) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok {
		select {
		case <-e.ready:
			delete(c.m, key)
		default:
		}
	}
	c.mu.Unlock()
}

// peek returns a completed, unexpired, error-free entry without computing
// anything — for callers that answer from cache now and warm in background.
func (c *cache) peek(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		select {
		case <-e.ready:
			if time.Now().Before(e.exp) && e.err == nil {
				return e.val, true
			}
		default:
		}
	}
	return nil, false
}

func (c *cache) do(key string, ttl time.Duration, fn func() (any, error)) (any, error) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok {
		select {
		case <-e.ready: // completed: fresh?
			if time.Now().Before(e.exp) {
				c.mu.Unlock()
				return e.val, e.err
			}
		default: // in flight: wait for it
			c.mu.Unlock()
			<-e.ready
			return e.val, e.err
		}
	}
	e := &entry{ready: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()

	e.val, e.err = fn()
	if e.err != nil {
		e.exp = time.Now().Add(errTTL)
	} else {
		e.exp = time.Now().Add(ttl)
	}
	close(e.ready)
	return e.val, e.err
}
