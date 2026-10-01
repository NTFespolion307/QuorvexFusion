// Package ratelimit provides a per-key (usually per-client-IP) token bucket,
// used to slow down guessing on the join and login endpoints.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type Limiter struct {
	mu      sync.Mutex
	every   time.Duration
	burst   int
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// New allows `burst` requests at once per key, refilling one every `every`.
func New(every time.Duration, burst int) *Limiter {
	return &Limiter{every: every, burst: burst, buckets: map[string]*bucket{}, lastGC: time.Now()}
}

// Allow reports whether a request for key may proceed now.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// Forget idle keys occasionally so the map can't grow without bound.
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 30*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}
