// SPDX-License-Identifier: GPL-3.0-or-later

// Package icewire drives the Phase 2 ICE negotiation over the broker-relayed signaling channel and
// returns the established direct QUIC connection. It is the glue between internal/ice (agent),
// the broker MsgSignal relay, and internal/directlink (QUIC-over-ICE). Transport-agnostic and
// loopback-testable: signals are exchanged via a send func + an incoming channel.
package icewire

import (
	"context"
	"crypto/tls"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/directlink"
	"github.com/sourjatilak/revquic/internal/ice"
)

// SignalSender sends one ice.Signal toward the peer (in practice, wrapped in a proto.MsgSignal and
// written to the broker control stream).
type SignalSender func(*ice.Signal) error

// Negotiate emits local credentials + trickles local candidates via send, consumes the peer's
// credentials/candidates from incoming, and once the peer credentials arrive establishes the direct
// QUIC connection over the ICE-nominated path (controlling dials, controlled accepts).
//
// incoming is drained until ctx is done or it is closed; candidates that arrive during connectivity
// checks are fed to the agent live (trickle ICE).
func Negotiate(ctx context.Context, agent ice.Agent, role ice.Role, sid string, send SignalSender, incoming <-chan *ice.Signal, tlsConf *tls.Config, p2pOnly bool) (quic.Connection, error) {
	creds, err := agent.LocalCredentials()
	if err != nil {
		return nil, err
	}
	if err := send(&ice.Signal{SessionID: sid, Kind: ice.SignalCredentials, Creds: &creds}); err != nil {
		return nil, err
	}

	agent.OnCandidate(func(c *ice.Candidate) {
		if c != nil {
			_ = send(&ice.Signal{SessionID: sid, Kind: ice.SignalCandidate, Candidate: c})
		}
	})
	if err := agent.GatherCandidates(); err != nil {
		return nil, err
	}

	remoteCreds := make(chan ice.Credentials, 1)
	go func() {
		sent := false
		for sig := range incoming {
			switch sig.Kind {
			case ice.SignalCredentials:
				if sig.Creds != nil && !sent {
					sent = true
					remoteCreds <- *sig.Creds
				}
			case ice.SignalCandidate:
				if sig.Candidate != nil {
					_ = agent.AddRemoteCandidate(*sig.Candidate)
				}
			}
		}
	}()

	var remote ice.Credentials
	select {
	case remote = <-remoteCreds:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return directlink.Establish(ctx, agent, role, remote, tlsConf, p2pOnly)
}
