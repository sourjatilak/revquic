// SPDX-License-Identifier: GPL-3.0-or-later

// Package ratelimit is a byte-oriented token-bucket limiter used for per-session bandwidth caps.
// A rate of <= 0 means unlimited. The clock is injectable for deterministic tests.
package ratelimit

import (
	"sync"
	"time"
)

// Bucket is a thread-safe token bucket measured in bytes.
type Bucket struct {
	mu     sync.Mutex
	rate   float64 // bytes per second; <= 0 means unlimited
	burst  float64 // max bytes that can accumulate
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewBucket creates a bucket allowing `rate` bytes/sec with a maximum burst of `burst` bytes.
// rate <= 0 disables limiting (Allow always returns true).
func NewBucket(rate, burst float64) *Bucket {
	if burst < rate {
		burst = rate
	}
	return &Bucket{rate: rate, burst: burst, tokens: burst, now: time.Now}
}

// Allow reports whether n bytes may be sent now, consuming tokens if so.
func (b *Bucket) Allow(n int) bool {
	if b == nil || b.rate <= 0 {
		return true // unlimited
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.last.IsZero() {
		b.last = now // establish baseline at first use (works with an injected clock)
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return true
	}
	return false
}
