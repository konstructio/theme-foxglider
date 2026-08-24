package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheSingleFlight(t *testing.T) {
	c := newCache()
	var calls int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.do("k", time.Minute, func() (any, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(20 * time.Millisecond)
				return "hit", nil
			})
			if err != nil || v.(string) != "hit" {
				t.Errorf("v=%v err=%v", v, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1", calls)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	c := newCache()
	n := 0
	fetch := func() (any, error) { n++; return n, nil }
	c.do("k", 10*time.Millisecond, fetch)
	v, _ := c.do("k", 10*time.Millisecond, fetch)
	if v.(int) != 1 {
		t.Fatalf("expected cached 1, got %v", v)
	}
	time.Sleep(15 * time.Millisecond)
	v, _ = c.do("k", 10*time.Millisecond, fetch)
	if v.(int) != 2 {
		t.Fatalf("expected refetched 2, got %v", v)
	}
}

func TestCacheErrorNotCachedLong(t *testing.T) {
	c := newCache()
	boom := errors.New("boom")
	n := 0
	_, err := c.do("k", time.Minute, func() (any, error) { n++; return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatal("want boom")
	}
	_, _ = c.do("k", time.Minute, func() (any, error) { n++; return nil, boom })
	if n != 1 { // within errTTL the error is served from cache
		t.Fatalf("fn ran %d times inside errTTL, want 1", n)
	}
}
