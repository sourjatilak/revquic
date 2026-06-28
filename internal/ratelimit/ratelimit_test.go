// SPDX-License-Identifier: GPL-3.0-or-later

package ratelimit

import (
	"testing"
	"time"
)

func TestBucketBurstExhaustRefill(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBucket(1000, 1000) // 1000 B/s, burst 1000
	b.now = func() time.Time { return now }

	if !b.Allow(600) {
		t.Fatal("first 600 should be allowed (burst)")
	}
	if b.Allow(600) {
		t.Fatal("second 600 should be denied (only 400 left)")
	}
	if !b.Allow(400) {
		t.Fatal("400 should fit the remaining tokens")
	}
	if b.Allow(1) {
		t.Fatal("bucket should be empty now")
	}
	// advance 1s -> +1000 tokens (capped at burst)
	now = now.Add(time.Second)
	if !b.Allow(1000) {
		t.Fatal("after 1s refill, 1000 should be allowed")
	}
	if b.Allow(1) {
		t.Fatal("should be empty again after consuming the full burst")
	}
	// refill is capped at burst even after a long idle
	now = now.Add(10 * time.Second)
	if !b.Allow(1000) {
		t.Fatal("capped refill should still allow exactly one burst")
	}
	if b.Allow(1) {
		t.Fatal("refill must be capped at burst (no accumulation beyond burst)")
	}
}

func TestUnlimited(t *testing.T) {
	b := NewBucket(0, 0)
	for i := 0; i < 1000; i++ {
		if !b.Allow(1 << 20) {
			t.Fatal("rate<=0 must always allow")
		}
	}
	var nilB *Bucket
	if !nilB.Allow(123) {
		t.Fatal("nil bucket must be unlimited")
	}
}
