// SPDX-License-Identifier: GPL-3.0-or-later

package ice

import (
	"context"
	"testing"
	"time"
)

// TestPionLoopbackConnect runs two real pion agents on the host (controlling + controlled),
// trickles host candidates between them, establishes an ICE connection, and verifies bytes flow.
// mDNS is disabled and loopback included so the test is self-contained (no STUN/TURN needed).
func TestPionLoopbackConnect(t *testing.T) {
	mk := func(role Role) Agent {
		a, err := NewPionAgent(PionConfig{Role: role, DisableMDNS: true, IncludeLoopback: true,
			FailedTimeout: 8 * time.Second})
		if err != nil {
			t.Fatalf("NewPionAgent(%s): %v", role, err)
		}
		return a
	}
	aAgent := mk(RoleControlling)
	bAgent := mk(RoleControlled)
	defer aAgent.Close()
	defer bAgent.Close()

	// Trickle candidates each way (ignore add errors for candidates that arrive post-connect).
	aAgent.OnCandidate(func(c *Candidate) {
		if c != nil {
			_ = bAgent.AddRemoteCandidate(*c)
		}
	})
	bAgent.OnCandidate(func(c *Candidate) {
		if c != nil {
			_ = aAgent.AddRemoteCandidate(*c)
		}
	})

	aCreds, err := aAgent.LocalCredentials()
	if err != nil {
		t.Fatal(err)
	}
	bCreds, err := bAgent.LocalCredentials()
	if err != nil {
		t.Fatal(err)
	}

	if err := aAgent.GatherCandidates(); err != nil {
		t.Fatal(err)
	}
	if err := bAgent.GatherCandidates(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	type res struct {
		conn Conn
		err  error
	}
	ach, bch := make(chan res, 1), make(chan res, 1)
	go func() { c, e := aAgent.Connect(ctx, bCreds); ach <- res{c, e} }() // controlling -> Dial
	go func() { c, e := bAgent.Connect(ctx, aCreds); bch <- res{c, e} }() // controlled  -> Accept

	ar, br := <-ach, <-bch
	if ar.err != nil || br.err != nil {
		t.Fatalf("connect failed: a=%v b=%v", ar.err, br.err)
	}

	// data flows A -> B
	if _, err := ar.conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = br.conn.(interface{ SetReadDeadline(time.Time) error }).SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	n, err := br.conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "ping" {
		t.Fatalf("got %q want ping", got)
	}
}
