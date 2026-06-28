// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/proto"
)

// newResumeBroker builds a broker with just the fields the resume logic needs (no userstore/qos/IP
// pool), so the park/resume decision can be tested in isolation.
func newResumeBroker() *broker {
	return &broker{
		parked:    map[string]*session{},
		exits:     map[string]*exitNode{},
		sessions:  map[uint64]*session{},
		resumeTTL: time.Hour,
	}
}

func parked(b *broker, key string, sid uint64, user, node string, age time.Duration) *session {
	s := &session{id: sid, username: user, nodeID: node, resumeKey: key, suspended: true, parkedAt: time.Now().Add(-age)}
	b.parked[key] = s
	return s
}

func TestClientClosedIntentionally(t *testing.T) {
	if clientClosedIntentionally(nil) {
		t.Errorf("nil error must not be intentional")
	}
	if clientClosedIntentionally(errors.New("timeout: no recent network activity")) {
		t.Errorf("a transient/timeout error must not be intentional (should park)")
	}
	// Other (e.g. sleep/wake) graceful close with code 0 must NOT count as intentional shutdown.
	if clientClosedIntentionally(&quic.ApplicationError{ErrorCode: 0}) {
		t.Errorf("code 0 close (reconnect/sleep) must not be intentional")
	}
	// The dedicated shutdown code IS intentional -> end the session, don't park.
	if !clientClosedIntentionally(&quic.ApplicationError{ErrorCode: quic.ApplicationErrorCode(proto.CloseClientShutdown)}) {
		t.Errorf("CloseClientShutdown code must be classified as intentional")
	}
}

func TestTryResume_Success(t *testing.T) {
	b := newResumeBroker()
	want := parked(b, "k1", 5, "alice", "exit-1", time.Minute)
	b.exits["exit-1"] = &exitNode{nodeID: "exit-1"}

	b.mu.Lock()
	s, ex := b.tryResume("k1", "alice", nil, nil) // unlocks on success
	if s == nil || ex == nil {
		b.mu.Unlock()
		t.Fatalf("expected resume to succeed")
	}
	if s != want {
		t.Errorf("resumed the wrong session")
	}
	if s.suspended {
		t.Errorf("resumed session should no longer be suspended")
	}
	if _, ok := b.parked["k1"]; ok {
		t.Errorf("session should be removed from parked on resume")
	}
	if _, ok := b.sessions[5]; !ok {
		t.Errorf("session should be back in the active map")
	}
	if ex.active.Load() != 1 {
		t.Errorf("exit active count = %d, want 1", ex.active.Load())
	}
}

func TestTryResume_Failures(t *testing.T) {
	cases := []struct {
		name      string
		key, user string
		age       time.Duration
		dropExit  bool
	}{
		{"expired", "k", "alice", 2 * time.Hour, false},
		{"wrong-user", "k", "bob", time.Minute, false},
		{"unknown-key", "nope", "alice", time.Minute, false},
		{"exit-offline", "k", "alice", time.Minute, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newResumeBroker()
			parked(b, "k", 9, "alice", "exit-1", c.age)
			if !c.dropExit {
				b.exits["exit-1"] = &exitNode{nodeID: "exit-1"}
			}
			b.mu.Lock()
			s, ex := b.tryResume(c.key, c.user, nil, nil) // keeps the lock held on failure
			b.mu.Unlock()
			if s != nil || ex != nil {
				t.Fatalf("expected no resume, got s=%v ex=%v", s, ex)
			}
		})
	}
}
