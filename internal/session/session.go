// SPDX-License-Identifier: GPL-3.0-or-later

// Package session ties the Phase 2 data-path state machine (internal/directpath) to the live
// datagram egress, so a session can start on the relay and atomically migrate to a direct path
// without dropping traffic. The TUN read loop calls Send; migration swaps the underlying path
// under a lock. This is transport-agnostic (a DatagramSender is satisfied by *quic.Connection).
package session

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/sourjatilak/revquic/internal/directpath"
	"github.com/sourjatilak/revquic/internal/ratelimit"
)

// ErrNoPath is returned by Send before a path is installed (or after Close).
var ErrNoPath = errors.New("session: no active data path")

// ErrRateLimited is returned by Send when the per-session byte budget is exceeded (packet dropped).
var ErrRateLimited = errors.New("session: rate limited")

// DatagramSender sends one datagram. Satisfied by quic.Connection.SendDatagram.
type DatagramSender interface {
	SendDatagram(b []byte) error
}

// Path is an egress: a datagram sender plus the per-path encoder applied to each IP packet.
// Relay paths prefix a session id (broker demuxes); the direct path is point-to-point (identity).
type Path struct {
	Sender DatagramSender
	Encode func(pkt []byte) []byte
}

// Identity is the encoder for the direct (point-to-point) path.
func Identity(b []byte) []byte { return b }

// Session owns one client's data-path lifecycle.
type Session struct {
	ID uint64

	m       *directpath.Machine
	mu      sync.RWMutex
	path    Path
	limiter *ratelimit.Bucket

	bytesSent atomic.Uint64
	pktsSent  atomic.Uint64
	drops     atomic.Uint64
}

// New creates a session in the New state (no path yet).
func New(id uint64) *Session {
	return &Session{ID: id, m: directpath.NewMachine()}
}

// SetRateLimit installs a per-session byte/sec limiter (rate <= 0 = unlimited). Safe to call once
// before traffic flows.
func (s *Session) SetRateLimit(bytesPerSec, burst float64) {
	s.mu.Lock()
	s.limiter = ratelimit.NewBucket(bytesPerSec, burst)
	s.mu.Unlock()
}

// State returns the current data-path state.
func (s *Session) State() directpath.PathState { return s.m.State() }

// IsDirect reports whether the session is currently on the direct (P2P) path.
func (s *Session) IsDirect() bool { return s.m.State() == directpath.StateDirect }

func (s *Session) setPath(p Path) {
	s.mu.Lock()
	s.path = p
	s.mu.Unlock()
}

// StartRelay installs the relay path and moves to Relaying (bootstrap; traffic begins).
func (s *Session) StartRelay(p Path) error {
	if _, err := s.m.Fire(directpath.EvStartRelay); err != nil {
		return err
	}
	s.setPath(p)
	return nil
}

// BeginChecks transitions to Checking (ICE running); the path stays on the relay meanwhile.
func (s *Session) BeginChecks() error {
	_, err := s.m.Fire(directpath.EvBeginChecks)
	return err
}

// UpgradeDirect atomically swaps to the direct path and moves to Direct (only valid from Checking).
func (s *Session) UpgradeDirect(p Path) error {
	if _, err := s.m.Fire(directpath.EvDirectEstablished); err != nil {
		return err
	}
	s.setPath(p)
	return nil
}

// ChecksFailed returns to Relaying (ICE failed/timed out); path unchanged (still relay).
func (s *Session) ChecksFailed() error {
	_, err := s.m.Fire(directpath.EvChecksFailed)
	return err
}

// FallbackRelay swaps back to the relay path and returns to Relaying (direct path lost).
func (s *Session) FallbackRelay(p Path) error {
	if _, err := s.m.Fire(directpath.EvDirectLost); err != nil {
		return err
	}
	s.setPath(p)
	return nil
}

// Close terminates the session and clears the path.
func (s *Session) Close() error {
	_, err := s.m.Fire(directpath.EvClose)
	s.setPath(Path{})
	return err
}

// Send encodes and sends one IP packet over the current path. Safe under concurrent migration.
func (s *Session) Send(pkt []byte) error {
	s.mu.RLock()
	p := s.path
	lim := s.limiter
	s.mu.RUnlock()
	if p.Sender == nil {
		return ErrNoPath
	}
	if !lim.Allow(len(pkt)) {
		s.drops.Add(1)
		return ErrRateLimited // drop: over the per-session byte budget
	}
	if err := p.Sender.SendDatagram(p.Encode(pkt)); err != nil {
		return err
	}
	s.bytesSent.Add(uint64(len(pkt)))
	s.pktsSent.Add(1)
	return nil
}

// Stats returns cumulative counters for this session: bytes sent (egress payload), packets sent,
// and packets dropped by the rate limiter. Safe for concurrent use.
func (s *Session) Stats() (bytes, packets, drops uint64) {
	return s.bytesSent.Load(), s.pktsSent.Load(), s.drops.Load()
}
