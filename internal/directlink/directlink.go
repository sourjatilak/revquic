// SPDX-License-Identifier: GPL-3.0-or-later

// Package directlink brings up the Phase 2 direct data path: it runs ICE to a connected pair, then
// establishes a QUIC connection (datagrams enabled) over that path. The controlling peer (A) dials;
// the controlled peer (C) listens and accepts. IP packets then ride QUIC datagrams (RFC 9221).
//
// See spec/phase2-direct-path.md §3.
package directlink

import (
	"context"
	"crypto/tls"
	"errors"
	"log"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/ice"
	"github.com/sourjatilak/revquic/internal/quicx"
)

// ErrRelayPair is returned by Establish when p2pOnly is set but ICE only managed a TURN-relayed
// candidate pair. Callers should treat it as "stay on the broker relay" (a relayed "direct" path
// usually underperforms a well-placed broker relay).
var ErrRelayPair = errors.New("directlink: ICE selected a TURN-relayed pair; p2p-only declined the upgrade")

// Establish runs ICE connectivity checks against the remote credentials, then opens a QUIC datagram
// connection over the nominated path. Role decides dial (controlling) vs listen/accept (controlled).
// If p2pOnly is set and the selected pair is TURN-relayed, it returns ErrRelayPair without upgrading.
func Establish(ctx context.Context, agent ice.Agent, role ice.Role, remote ice.Credentials, tlsConf *tls.Config, p2pOnly bool) (quic.Connection, error) {
	conn, err := agent.Connect(ctx, remote)
	if err != nil {
		return nil, err
	}
	if lt, rt, ok := agent.SelectedPair(); ok {
		log.Printf("ICE pair: local=%s remote=%s", lt, rt)
		if p2pOnly && (lt == ice.CandidateRelay || rt == ice.CandidateRelay) {
			_ = conn.Close()
			return nil, ErrRelayPair
		}
	}
	pc, remoteAddr := ice.NewPacketConn(conn, conn.RemoteAddr().String())

	if role == ice.RoleControlling {
		qc, err := quicx.DialDatagram(ctx, pc, remoteAddr, tlsConf)
		if err != nil {
			_ = pc.Close()
			return nil, err
		}
		return qc, nil
	}

	ln, err := quicx.ListenDatagram(pc, tlsConf)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	qc, err := ln.Accept(ctx)
	if err != nil {
		_ = ln.Close()
		_ = pc.Close()
		return nil, err
	}
	return qc, nil
}
