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
