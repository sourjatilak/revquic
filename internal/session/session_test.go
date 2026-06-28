// SPDX-License-Identifier: GPL-3.0-or-later

package session

import (
	"encoding/binary"
	"sync"
	"testing"

	"github.com/sourjatilak/revquic/internal/directpath"
)

// recordSender captures datagrams sent to it.
type recordSender struct {
	mu   sync.Mutex
	last []byte
	n    int
}

func (r *recordSender) SendDatagram(b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = append([]byte(nil), b...)
	r.n++
	return nil
}

func relayEncode(sid uint64) func([]byte) []byte {
	return func(p []byte) []byte {
		out := make([]byte, 8+len(p))
		binary.BigEndian.PutUint64(out[:8], sid)
		copy(out[8:], p)
		return out
	}
}

func TestSessionMigration(t *testing.T) {
	relay := &recordSender{}
	direct := &recordSender{}
	s := New(7)

	if err := s.Send([]byte("x")); err != ErrNoPath {
		t.Fatalf("send before path: want ErrNoPath, got %v", err)
	}

	// bootstrap on relay
	if err := s.StartRelay(Path{Sender: relay, Encode: relayEncode(7)}); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	if s.State() != directpath.StateRelaying {
		t.Fatalf("state=%s", s.State())
	}
	if err := s.Send([]byte("hi")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if relay.n != 1 || len(relay.last) != 10 || binary.BigEndian.Uint64(relay.last[:8]) != 7 {
		t.Fatalf("relay datagram wrong: n=%d last=%v", relay.n, relay.last)
	}

	// ICE checks, then upgrade to direct
	if err := s.BeginChecks(); err != nil {
		t.Fatalf("begin checks: %v", err)
	}
	if err := s.UpgradeDirect(Path{Sender: direct, Encode: Identity}); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if s.State() != directpath.StateDirect {
		t.Fatalf("state=%s", s.State())
	}
	if err := s.Send([]byte("yo")); err != nil {
		t.Fatalf("send direct: %v", err)
	}
	if direct.n != 1 || string(direct.last) != "yo" {
		t.Fatalf("direct datagram wrong: n=%d last=%q", direct.n, direct.last)
	}
	if relay.n != 1 {
		t.Fatalf("relay should not receive after migration: n=%d", relay.n)
	}

	// direct lost -> fall back to relay
	if err := s.FallbackRelay(Path{Sender: relay, Encode: relayEncode(7)}); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if s.State() != directpath.StateRelaying {
		t.Fatalf("state=%s", s.State())
	}
	_ = s.Send([]byte("z"))
	if relay.n != 2 {
		t.Fatalf("relay should receive after fallback: n=%d", relay.n)
	}
}

func TestSessionRateLimit(t *testing.T) {
	rec := &recordSender{}
	s := New(9)
	if err := s.StartRelay(Path{Sender: rec, Encode: Identity}); err != nil {
		t.Fatal(err)
	}
	s.SetRateLimit(100, 100) // 100 B/s, burst 100

	if err := s.Send(make([]byte, 100)); err != nil {
		t.Fatalf("first 100B should pass: %v", err)
	}
	if err := s.Send([]byte{0}); err != ErrRateLimited {
		t.Fatalf("over-budget send: want ErrRateLimited, got %v", err)
	}
	if rec.n != 1 {
		t.Fatalf("only the first send should reach the sender, n=%d", rec.n)
	}
}

func TestSessionInvalidUpgrade(t *testing.T) {
	s := New(1)
	_ = s.StartRelay(Path{Sender: &recordSender{}, Encode: Identity})
	// cannot upgrade without BeginChecks
	if err := s.UpgradeDirect(Path{Sender: &recordSender{}, Encode: Identity}); err == nil {
		t.Fatal("expected invalid transition Relaying->UpgradeDirect")
	}
	_ = s.Close()
	if err := s.Send([]byte("x")); err != ErrNoPath {
		t.Fatalf("after close: want ErrNoPath, got %v", err)
	}
}
