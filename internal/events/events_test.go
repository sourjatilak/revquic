// SPDX-License-Identifier: GPL-3.0-or-later

package events

import (
	"testing"
	"time"

	"github.com/sourjatilak/revquic/internal/adminapi"
)

func TestBusPublishSubscribe(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish(adminapi.Event{Type: adminapi.EvNodeConnected, NodeID: "exit-1"})

	select {
	case ev := <-ch:
		if ev.Type != adminapi.EvNodeConnected || ev.NodeID != "exit-1" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBusUnsubscribeStopsDelivery(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	cancel() // channel is closed by cancel

	// publishing after cancel must not panic and the channel must be drained/closed
	b.Publish(adminapi.Event{Type: adminapi.EvSessionEnded})
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel after cancel")
	}
}
