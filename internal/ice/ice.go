// SPDX-License-Identifier: GPL-3.0-or-later

// Package ice defines the ICE agent seam used by the client (A) and exit (C) for the Phase 2 direct
// path. It has NO external dependencies so it compiles and is mockable offline; the production
// implementation (pion_adapter.go, not present in offline builds) wraps github.com/pion/ice/v2.
//
// See spec/phase2-direct-path.md. Wiring overview for the pion adapter:
//
//	cfg := &ice.AgentConfig{
//	    URLs:             stunTurnURLs,          // B's STUN + TURN (REST creds)
//	    CandidateTypes:   {Host, ServerReflexive, Relay},
//	    MulticastDNSMode: QueryAndGather,        // hide host IPs
//	    KeepaliveInterval: 2s, FailedTimeout: 10s,
//	}
//	agent, _ := ice.NewAgent(cfg)
//	agent.OnCandidate(func(c){ signalToBroker(c.Marshal()) })   // trickle
//	agent.GatherCandidates()
//	// A (controlling): conn,_ := agent.Dial(ctx, remoteUfrag, remotePwd)
//	// C (controlled):  conn,_ := agent.Accept(ctx, remoteUfrag, remotePwd)
//	// then run a quic.Transport over the SAME *net.UDPConn (ReadNonQUICPacket demux).
package ice

import (
	"context"
	"net"
)

// Role is the ICE controlling/controlled role (RFC 8445). The dialer (A) is controlling.
type Role string

const (
	RoleControlling Role = "controlling"
	RoleControlled  Role = "controlled"
)

// CandidateType enumerates ICE candidate kinds.
type CandidateType string

const (
	CandidateHost            CandidateType = "host"
	CandidateServerReflexive CandidateType = "srflx"
	CandidatePeerReflexive   CandidateType = "prflx"
	CandidateRelay           CandidateType = "relay"
)

// Candidate is a transport-encodable ICE candidate. Marshal/Unmarshal cross the signaling channel.
type Candidate struct {
	Type       CandidateType `json:"type"`
	Foundation string        `json:"foundation,omitempty"`
	Protocol   string        `json:"protocol,omitempty"` // udp
	Address    string        `json:"address"`
	Port       int           `json:"port"`
	Priority   uint32        `json:"priority,omitempty"`
	Raw        string        `json:"raw,omitempty"` // pion's c.Marshal() string, when available
}

// Credentials are the ufrag/pwd a peer needs to run checks against this agent.
type Credentials struct {
	Ufrag string `json:"ufrag"`
	Pwd   string `json:"pwd"`
}

// Conn is the connected, NAT-traversed path ICE produces (a net.Conn over the nominated pair).
// It is aliased to net.Conn so it carries deadlines + addresses for the QUIC PacketConn adapter.
type Conn = net.Conn

// Agent abstracts an ICE agent so the data-plane code (and tests) do not depend on pion directly.
type Agent interface {
	// LocalCredentials returns this agent's ufrag/pwd to send to the peer via signaling.
	LocalCredentials() (Credentials, error)
	// OnCandidate registers a trickle callback; a nil candidate signals gathering complete.
	OnCandidate(func(*Candidate))
	// GatherCandidates starts asynchronous candidate gathering.
	GatherCandidates() error
	// AddRemoteCandidate feeds a peer candidate received over signaling.
	AddRemoteCandidate(Candidate) error
	// Connect runs connectivity checks against the peer creds and returns the chosen path.
	// Controlling agents nominate; controlled agents accept. Role is fixed at construction.
	Connect(ctx context.Context, remote Credentials) (Conn, error)
	// Restart re-gathers and re-checks after a path loss (ICE restart).
	Restart() error
	// SelectedPair returns the negotiated local/remote candidate types after Connect (ok=false if none).
	SelectedPair() (local, remote CandidateType, ok bool)
	Close() error
}

// SignalKind tags messages on the broker-mediated signaling channel.
type SignalKind string

const (
	SignalCandidate   SignalKind = "candidate"
	SignalCredentials SignalKind = "credentials"
	SignalRole        SignalKind = "role"
	SignalEnd         SignalKind = "end" // end-of-candidates
)

// Signal is one message exchanged via the broker between A and C (bound to a session id + nonce).
type Signal struct {
	SessionID string       `json:"sessionId"`
	Nonce     string       `json:"nonce,omitempty"`
	Kind      SignalKind   `json:"kind"`
	Candidate *Candidate   `json:"candidate,omitempty"`
	Creds     *Credentials `json:"creds,omitempty"`
	Role      Role         `json:"role,omitempty"`
}
