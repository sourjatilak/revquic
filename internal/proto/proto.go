// SPDX-License-Identifier: GPL-3.0-or-later

// Package proto defines the Revquic Phase 0 control protocol and data-plane datagram codec.
//
// Phase 0 scope (see ../../README.md): prove L3-over-QUIC-datagram relay end to end.
//   - Control plane: length-prefixed JSON messages on a single bidirectional QUIC stream.
//   - Data plane: each IP packet is carried in ONE QUIC DATAGRAM, prefixed with an 8-byte
//     big-endian session id so the broker can demultiplex many sessions over one exit
//     connection. Datagrams are unreliable + unordered — correct for tunneled IP packets
//     (never carry IP packets on a reliable QUIC stream; see spec/reconciliation-and-validation.md §3).
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// ALPN is the QUIC application protocol identifier for the Phase 0 spike.
const ALPN = "revquic-phase0"

// CloseClientShutdown is the QUIC application error code a client uses when it closes its broker
// connection due to an INTENTIONAL exit (Ctrl-C / SIGTERM). The broker uses it to distinguish a
// deliberate disconnect (end the session now — the client won't return) from a transient drop or a
// sleep/wake reconnect (park the session so it can resume). Values cast to quic.ApplicationErrorCode.
const CloseClientShutdown uint64 = 4200

// MsgType enumerates control-plane message types.
type MsgType string

const (
	// Exit (C) -> Broker (B): announce this exit node.
	MsgRegister MsgType = "register"
	// B -> C: registration accepted.
	MsgRegisterOK MsgType = "register_ok"
	// B -> C: serve a new session for a client; includes the client's assigned VPN IP.
	MsgSessionStart MsgType = "session_start"
	// B -> C: a client session ended (client disconnected); the exit tears down its session state.
	MsgSessionEnd MsgType = "session_end"
	// B -> C: the client went offline but its session is parked (resumable); the exit keeps the
	// session and NAT state but marks it suspended until a resume (MsgSessionStart) or MsgSessionEnd.
	MsgSessionSuspend MsgType = "session_suspend"
	// Client (A) -> B: request egress in a region.
	MsgConnect MsgType = "connect"
	// B -> A: session established; carries assigned VPN IP + netmask + tunnel MTU.
	MsgConnectOK MsgType = "connect_ok"
	// Any -> any: error.
	MsgError MsgType = "error"
	// A<->B<->C: ICE signaling relay (candidate/creds/role trickle). Payload is opaque JSON
	// (an ice.Signal) so this package stays dependency-free. Bound to SessionID (+ Nonce).
	MsgSignal MsgType = "signal"
	// A->B and C->B: periodic per-session QoS telemetry (throughput, byte totals, drops, RTT).
	// Lets the broker observe the DIRECT path (which bypasses it) and quality metrics it can't see.
	MsgReport MsgType = "report"
	// Endpoint->B: latency probe. B replies MsgPong echoing TS so the endpoint can measure RTT.
	MsgPing MsgType = "ping"
	// B->endpoint: reply to MsgPing (echoes TS).
	MsgPong MsgType = "pong"
	// C->B: periodic exit-node host utilization (CPU/mem/disk) for the admin dashboard.
	MsgNodeStatus MsgType = "node_status"
)

// ExitInfo describes one selectable exit (returned to clients via ListExits).
type ExitInfo struct {
	NodeID      string `json:"nodeId"`
	Name        string `json:"name,omitempty"`
	Region      string `json:"region"`
	System      string `json:"system,omitempty"`
	ActiveUsers int    `json:"activeUsers"`
	Capacity    int    `json:"capacity"`
}

// Control is the single envelope for all control-plane messages (JSON).
type Control struct {
	Type MsgType `json:"type"`

	// Auth (Phase 1): node shared-secret on Register; client user token on Connect.
	Token string `json:"token,omitempty"`

	// Register / RegisterOK
	NodeID   string `json:"nodeId,omitempty"`
	Region   string `json:"region,omitempty"`
	Capacity int    `json:"capacity,omitempty"`
	// Name is an optional human-friendly display name for the endpoint (exit on Register, client on
	// Connect). The admin dashboard shows it alongside the id.
	Name string `json:"name,omitempty"`

	// Connect
	RequestedRegion string `json:"requestedRegion,omitempty"`
	// RequestedExit (optional): pin to a specific exit nodeId (manual selection). Empty = auto (LB).
	RequestedExit string `json:"requestedExit,omitempty"`
	// ResumeKey (optional): a stable per-client-process key. If the broker has a parked session for
	// this key (client reconnecting within the resume TTL), it resumes that session (same exit + VPN
	// IP) instead of allocating a new one.
	ResumeKey string `json:"resumeKey,omitempty"`
	// ListExits: client asks the broker to reply with the available exits in the region (no session).
	ListExits bool `json:"listExits,omitempty"`
	// ExitList: broker->client reply to ListExits (the region's exits the client may pick from).
	ExitList []ExitInfo `json:"exitList,omitempty"`

	// SessionStart / ConnectOK
	SessionID uint64 `json:"sessionId,omitempty"`
	ClientIP  string `json:"clientIp,omitempty"` // e.g. 10.99.0.2
	Netmask   string `json:"netmask,omitempty"`  // e.g. 255.255.255.0
	MTU       int    `json:"mtu,omitempty"`      // tunnel MTU (datagram-safe)

	// STUN/TURN (Phase 2): ICE servers + minted short-lived TURN REST creds for this session.
	StunURL  string `json:"stunUrl,omitempty"`
	TurnURL  string `json:"turnUrl,omitempty"`
	TurnUser string `json:"turnUser,omitempty"`
	TurnPass string `json:"turnPass,omitempty"`

	// Error
	Error string `json:"error,omitempty"`

	// Signal (MsgSignal): opaque JSON of an ice.Signal, relayed between A and C by the broker.
	Signal []byte `json:"signal,omitempty"`
	Nonce  string `json:"nonce,omitempty"`

	// Report (MsgReport): per-session QoS telemetry from a client/exit. Bound to SessionID.
	BytesUp       uint64  `json:"bytesUp,omitempty"`
	BytesDown     uint64  `json:"bytesDown,omitempty"`
	ThroughputBps float64 `json:"throughputBps,omitempty"`
	Drops         uint64  `json:"drops,omitempty"`
	RTTms         int     `json:"rttMs,omitempty"`
	Direct        bool    `json:"direct,omitempty"`
	// Enriched realtime telemetry: reporting endpoint's host, its TUN interface, the active path
	// ("relay"/"direct"), and whether STUN/TURN are in use on the direct path.
	Host      string `json:"host,omitempty"`
	TunName   string `json:"tunName,omitempty"`
	PathKind  string `json:"pathKind,omitempty"`
	UsingStun bool   `json:"usingStun,omitempty"`
	UsingTurn bool   `json:"usingTurn,omitempty"`
	// OS is the endpoint's "GOOS/GOARCH" system info (sent on Register by exits and in reports).
	OS string `json:"os,omitempty"`

	// NodeStatus (MsgNodeStatus): exit host utilization percentages (0..100).
	CPUPct  float64 `json:"cpuPct,omitempty"`
	MemPct  float64 `json:"memPct,omitempty"`
	DiskPct float64 `json:"diskPct,omitempty"`

	// Ping/Pong (MsgPing/MsgPong): unix-nanos timestamp echoed back to measure RTT.
	TS int64 `json:"ts,omitempty"`
}

// WriteControl writes a length-prefixed JSON control message.
func WriteControl(w io.Writer, m *Control) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("control message too large: %d", len(b))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadControl reads a length-prefixed JSON control message.
func ReadControl(r io.Reader) (*Control, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 1<<20 {
		return nil, fmt.Errorf("control message too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var m Control
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// DatagramHeaderLen is the size of the session-id prefix on every data-plane datagram.
const DatagramHeaderLen = 8

// SafeTunnelMTU is the inner tunnel MTU advertised to clients and used by the exit's TUN. It must
// be small enough that an inner IP packet plus the DatagramHeaderLen prefix fits inside a single
// QUIC DATAGRAM frame. quic-go's initial max datagram payload is ~1240 bytes (InitialPacketSize
// 1280 minus QUIC packet/frame overhead, before PMTU discovery), so 1200 + 8 = 1208 leaves a safe
// margin and matches the conventional VPN MTU. Going higher (e.g. 1350) overflows the initial
// datagram size and yields "DATAGRAM frame too large".
const SafeTunnelMTU = 1200

// EncodeDatagram prefixes an IP packet with its session id.
func EncodeDatagram(sessionID uint64, pkt []byte) []byte {
	out := make([]byte, DatagramHeaderLen+len(pkt))
	binary.BigEndian.PutUint64(out[:DatagramHeaderLen], sessionID)
	copy(out[DatagramHeaderLen:], pkt)
	return out
}

// DecodeDatagram splits a datagram into its session id and IP packet.
// The returned packet slice aliases the input buffer.
func DecodeDatagram(b []byte) (sessionID uint64, pkt []byte, err error) {
	if len(b) < DatagramHeaderLen {
		return 0, nil, fmt.Errorf("short datagram: %d bytes", len(b))
	}
	sessionID = binary.BigEndian.Uint64(b[:DatagramHeaderLen])
	return sessionID, b[DatagramHeaderLen:], nil
}
