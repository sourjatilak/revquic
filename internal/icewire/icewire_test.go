// SPDX-License-Identifier: GPL-3.0-or-later

package icewire

import (
	"context"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/ice"
	"github.com/sourjatilak/revquic/internal/quicx"
)

// TestNegotiateLoopback wires two icewire.Negotiate calls together via in-memory signal channels
// (standing in for the broker relay) and verifies a direct QUIC datagram link is established.
func TestNegotiateLoopback(t *testing.T) {
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

	a2c := make(chan *ice.Signal, 64)
	c2a := make(chan *ice.Signal, 64)
	aSend := func(s *ice.Signal) error { a2c <- s; return nil }
	cSend := func(s *ice.Signal) error { c2a <- s; return nil }

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
		qc, e := Negotiate(ctx, aAgent, ice.RoleControlling, "sid-1", aSend, c2a, quicx.ClientTLS(), false)
		ach <- res{qc, e}
	}()
	go func() {
		qc, e := Negotiate(ctx, cAgent, ice.RoleControlled, "sid-1", cSend, a2c, serverTLS, false)
		cch <- res{qc, e}
	}()

	ar, cr := <-ach, <-cch
	if ar.err != nil || cr.err != nil {
		t.Fatalf("negotiate: a=%v c=%v", ar.err, cr.err)
	}
	if err := ar.qc.SendDatagram([]byte("direct-ok")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := cr.qc.ReceiveDatagram(ctx)
	if err != nil || string(got) != "direct-ok" {
		t.Fatalf("recv: %q err=%v", got, err)
	}
	_ = ar.qc.CloseWithError(0, "done")
	_ = cr.qc.CloseWithError(0, "done")
}
