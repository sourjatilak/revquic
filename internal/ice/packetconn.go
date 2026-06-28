// SPDX-License-Identifier: GPL-3.0-or-later

package ice

import (
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

// packetConn adapts a connected ICE path (net.Conn over the nominated pair) to a net.PacketConn,
// so quic-go can run a Transport over it. All reads come from / writes go to the single ICE peer;
// the peer address is synthetic but stable (quic-go only needs a consistent remote).
type packetConn struct {
	conn   net.Conn
	remote net.Addr
	local  net.Addr
}

// pktAddr is a stable synthetic address for the single ICE peer / local endpoint.
type pktAddr struct{ s string }

func (a pktAddr) Network() string { return "ice" }
func (a pktAddr) String() string  { return a.s }

// pcSeq makes each packetConn's LocalAddr unique. quic-go keys a global connection multiplexer on
// LocalAddr: it PANICS if two conns share the same index, and it dereferences LocalAddr() on
// teardown (crashing if it ever returns nil). So LocalAddr must be stable, non-nil, and unique —
// the underlying pion conn's LocalAddr can go nil after Close, hence this synthetic address.
var pcSeq atomic.Uint64

// NewPacketConn wraps an ICE Conn as a net.PacketConn for QUIC and returns the synthetic remote
// address to dial (it must equal the addr ReadFrom reports so quic-go matches the connection).
func NewPacketConn(conn Conn, peerAddr string) (net.PacketConn, net.Addr) {
	if peerAddr == "" {
		peerAddr = "ice-peer"
	}
	remote := pktAddr{s: peerAddr}
	local := pktAddr{s: "ice-local-" + strconv.FormatUint(pcSeq.Add(1), 10)}
	return &packetConn{conn: conn, remote: remote, local: local}, remote
}

func (p *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.conn.Read(b)
	return n, p.remote, err
}

func (p *packetConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return p.conn.Write(b)
}

func (p *packetConn) Close() error                       { return p.conn.Close() }
func (p *packetConn) LocalAddr() net.Addr                { return p.local }
func (p *packetConn) SetDeadline(t time.Time) error      { return p.conn.SetDeadline(t) }
func (p *packetConn) SetReadDeadline(t time.Time) error  { return p.conn.SetReadDeadline(t) }
func (p *packetConn) SetWriteDeadline(t time.Time) error { return p.conn.SetWriteDeadline(t) }

// RemoteAddr exposes the synthetic peer address (used as the dial/remote addr for quic-go).
func (p *packetConn) RemoteAddr() net.Addr { return p.remote }
