// SPDX-License-Identifier: GPL-3.0-or-later

// Package events is an in-process pub/sub bus for broker state changes (node connect/disconnect,
// session start/end). The admin /events SSE endpoint fans these out to browsers.
package events

import (
	"sync"

	"github.com/sourjatilak/revquic/internal/adminapi"
)

// Bus is a fan-out publisher. Subscribers get a buffered channel; slow subscribers that fill
// their buffer drop events (presence is reconciled from a fresh snapshot on reconnect).
type Bus struct {
	mu   sync.RWMutex
	subs map[int]chan adminapi.Event
	next int
}

// NewBus creates an empty bus.
func NewBus() *Bus { return &Bus{subs: map[int]chan adminapi.Event{}} }

// Subscribe returns a receive channel and an unsubscribe function.
func (b *Bus) Subscribe() (<-chan adminapi.Event, func()) {
	ch := make(chan adminapi.Event, 64)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// Publish delivers an event to all current subscribers (non-blocking; drops on full buffers).
func (b *Bus) Publish(ev adminapi.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow consumer: drop; it will re-snapshot on reconnect
		}
	}
}
