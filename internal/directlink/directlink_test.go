// SPDX-License-Identifier: GPL-3.0-or-later

package directlink

import (
	"context"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/ice"
	"github.com/sourjatilak/revquic/internal/quicx"
)

// TestDirectLinkLoopback proves the Phase 2 direct path end to end: two ICE agents connect over
// loopback host candidates, then a QUIC datagram connection is established over the nominated path
// and IP-packet-sized datagrams round-trip both ways.
func TestDirectLinkLoopback(t *testing.T) {
	mk := func(role ice.Role) ice.Agent {
		a, err := ice.NewPionAgent(ice.PionConfig{Role: role, DisableMDNS: true, IncludeLoopback: true,
			FailedTimeout: 8 * time.Second})
		if err != nil {
			t.Fatalf("agent: %v", err)
		}
		return a
	}
	aAgent := mk(ice.RoleControlling)
	cAgent := mk(ice.RoleControlled)
	defer aAgent.Close()
	defer cAgent.Close()

	aAgent.OnCandidate(func(c *ice.Candidate) {
		if c != nil {
			_ = cAgent.AddRemoteCandidate(*c)
		}
	})
	cAgent.OnCandidate(func(c *ice.Candidate) {
		if c != nil {
			_ = aAgent.AddRemoteCandidate(*c)
		}
	})
	aCreds, _ := aAgent.LocalCredentials()
	cCreds, _ := cAgent.LocalCredentials()
	if err := aAgent.GatherCandidates(); err != nil {
		t.Fatal(err)
	}
	if err := cAgent.GatherCandidates(); err != nil {
		t.Fatal(err)
	}

	serverTLS, err := quicx.ServerTLS()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type res struct {
		qc  quic.Connection
		err error
	}
	ach, cch := make(chan res, 1), make(chan res, 1)
	go func() {
		qc, e := Establish(ctx, aAgent, ice.RoleControlling, cCreds, quicx.ClientTLS(), false)
		ach <- res{qc, e}
	}()
	go func() {
		qc, e := Establish(ctx, cAgent, ice.RoleControlled, aCreds, serverTLS, false)
		cch <- res{qc, e}
	}()

	ar, cr := <-ach, <-cch
	if ar.err != nil || cr.err != nil {
		t.Fatalf("establish: a=%v c=%v", ar.err, cr.err)
	}

	// A -> C datagram (simulated IP packet)
	if err := ar.qc.SendDatagram([]byte("packet-A-to-C")); err != nil {
		t.Fatalf("A SendDatagram: %v", err)
	}
	got, err := cr.qc.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("C ReceiveDatagram: %v", err)
	}
	if string(got) != "packet-A-to-C" {
		t.Fatalf("C got %q", got)
	}

	// C -> A datagram (reply path)
	if err := cr.qc.SendDatagram([]byte("packet-C-to-A")); err != nil {
		t.Fatalf("C SendDatagram: %v", err)
	}
	got, err = ar.qc.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("A ReceiveDatagram: %v", err)
	}
	if string(got) != "packet-C-to-A" {
		t.Fatalf("A got %q", got)
	}

	_ = ar.qc.CloseWithError(0, "done")
	_ = cr.qc.CloseWithError(0, "done")
}
