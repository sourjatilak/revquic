// SPDX-License-Identifier: GPL-3.0-or-later

// Command revquic-exit is the Revquic exit node (C).
//
// It dials the broker (reverse tunnel), registers a region, and for each client session bootstraps
// on the relay (writes received IP-packet datagrams to a shared TUN, masquerades to the internet).
// With -direct it runs ICE (controlled) per session over the broker-relayed signaling channel and
// migrates that session's egress onto a direct QUIC path. Linux + root (TUN + iptables).
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/conf"
	"github.com/sourjatilak/revquic/internal/directlink"
	"github.com/sourjatilak/revquic/internal/ice"
	"github.com/sourjatilak/revquic/internal/icewire"
	"github.com/sourjatilak/revquic/internal/logx"
	"github.com/sourjatilak/revquic/internal/netcfg"
	"github.com/sourjatilak/revquic/internal/proto"
	"github.com/sourjatilak/revquic/internal/quicx"
	"github.com/sourjatilak/revquic/internal/session"
	"github.com/sourjatilak/revquic/internal/shutdown"
	"github.com/sourjatilak/revquic/internal/sysstat"
	"github.com/sourjatilak/revquic/internal/telemetry"
	"github.com/sourjatilak/revquic/internal/tunnel"
)

type ctrlWriter struct {
	mu sync.Mutex
	s  quic.Stream
}

func (c *ctrlWriter) write(m *proto.Control) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return proto.WriteControl(c.s, m)
}

// WriteControl satisfies telemetry.CtrlWriter.
func (c *ctrlWriter) WriteControl(m *proto.Control) error { return c.write(m) }

type exitSession struct {
	sess                           *session.Session
	incoming                       chan *ice.Signal
	clientIP                       string
	stun, turn, turnUser, turnPass string // broker-provided per-session (may be empty)
	rtt                            *telemetry.RTT

	startedAt   time.Time
	suspended   bool // client offline but session parked (resumable); set by MsgSessionSuspend
	suspendedAt time.Time
	bytesUp     atomic.Uint64 // client -> internet (written to TUN); downlink is sess.Stats()
	bpsUpBits   atomic.Uint64 // smoothed uplink throughput (float64 bits), set by the sampler
	bpsDownBits atomic.Uint64 // smoothed downlink throughput (float64 bits)
}

type exit struct {
	conn quic.Connection
	tun  *tunnel.Device
	cw   *ctrlWriter

	connCtx context.Context // current broker connection's context (cancelled when it drops)

	nodeID, displayName, region string // identity, for the local status page

	direct                         bool
	directMode                     string
	stun, turn, turnUser, turnPass string
	iceKeepalive                   time.Duration
	directServerTLS                *tls.Config
	rateBytes                      float64
	reportInterval                 time.Duration
	ctx                            context.Context

	mu       sync.RWMutex
	sessions map[uint64]*exitSession
	byIP     map[netip.Addr]uint64
}

func main() {
	brokerAddr := flag.String("broker", "localhost:4242", "broker QUIC address")
	configPath := flag.String("config", "", "path to a key=value config file (CLI flags take precedence)")
	logFile := flag.String("log-file", "", "write logs to this file (default: stderr)")
	logType := flag.String("log-type", "text", "log format: text | json")
	nodeID := flag.String("nodeId", "", "this node's id (default: derived from -name, else a random exit-<n>)")
	name := flag.String("name", "", "optional human-friendly display name for this exit (shown in the dashboard)")
	statusAddr := flag.String("status-addr", "127.0.0.1:8085", "local status web UI address (no auth; bind to localhost only); empty or 'off' disables")
	region := flag.String("region", "us-west", "region this exit serves")
	capacity := flag.Int("capacity", 0, "max concurrent client sessions this exit accepts (0 = broker default of 100)")
	uplink := flag.String("uplink", "eth0", "WAN interface to masquerade out of")
	gwIP := flag.String("gw", "10.99.0.1/24", "exit-side TUN gateway CIDR")
	token := flag.String("token", "node-secret", "shared node secret expected by the broker")
	isolate := flag.Bool("isolate", true, "per-session isolation: block client-to-client and client-to-LAN traffic")
	rateBytes := flag.Float64("rate-bytes", 0, "per-session egress cap in bytes/sec (0 = unlimited)")
	reportInterval := flag.Duration("report-interval", 5*time.Second, "QoS report cadence to the broker (0 disables)")
	direct := flag.Bool("direct", false, "attempt direct ICE/QUIC paths per session")
	directMode := flag.String("direct-mode", "any", "with -direct: 'any' | 'p2p-only' (only accept true peer-to-peer upgrades; reject TURN-relayed)")
	iceKeepalive := flag.Duration("ice-keepalive", time.Second, "with -direct: STUN keepalive cadence on the ICE pair (keeps the NAT binding fresh; lower = more resilient on short-timeout NATs)")
	stun := flag.String("stun", "", "comma-separated STUN URLs")
	turn := flag.String("turn", "", "comma-separated TURN URLs")
	turnUser := flag.String("turn-user", "", "TURN username")
	turnPass := flag.String("turn-pass", "", "TURN password")
	tlsCA := flag.String("tls-ca", "", "CA cert PEM; with -tls-cert/-tls-key enables mTLS to the broker")
	tlsCert := flag.String("tls-cert", "", "node leaf cert PEM (mTLS)")
	tlsKey := flag.String("tls-key", "", "node leaf key PEM (mTLS)")
	tlsServerName := flag.String("tls-server-name", "broker", "expected broker cert SAN (mTLS)")
	flag.Parse()
	if *configPath != "" {
		if err := conf.ApplyFile(flag.CommandLine, *configPath); err != nil {
			log.Fatalf("config: %v", err)
		}
	}
	logClose, err := logx.Setup("exit", *logFile, *logType)
	if err != nil {
		log.Fatalf("log: %v", err)
	}
	defer logClose()

	ctx := context.Background()
	brokerTLS := quicx.ClientTLS()
	if *tlsCA != "" && *tlsCert != "" && *tlsKey != "" {
		mtls, err := quicx.ClientMTLSFromFiles(*tlsCA, *tlsCert, *tlsKey, *tlsServerName)
		if err != nil {
			log.Fatalf("mTLS: %v", err)
		}
		brokerTLS = mtls
		log.Printf("control-plane mTLS to broker enabled")
	}
	tun, err := tunnel.Open()
	if err != nil {
		log.Fatalf("tun: %v", err)
	}
	defer tun.Close()
	if err := netcfg.AddrUp(tun.Name(), *gwIP, proto.SafeTunnelMTU); err != nil {
		log.Fatalf("addr: %v", err)
	}
	if err := netcfg.EnableForwarding(); err != nil {
		log.Fatalf("forwarding: %v", err)
	}
	if err := netcfg.Masquerade("10.99.0.0/24", *uplink); err != nil {
		log.Fatalf("masquerade: %v", err)
	}
	if *isolate {
		if err := netcfg.Isolate("10.99.0.0/24"); err != nil {
			log.Fatalf("isolate: %v", err)
		}
		log.Printf("per-session isolation enabled (no client-to-client or client-to-LAN)")
	}
	log.Printf("tun %s up (%s), masquerading 10.99.0.0/24 -> %s", tun.Name(), *gwIP, *uplink)

	nodeIDEff := deriveNodeID(*nodeID, *name)
	e := &exit{
		tun:    tun,
		nodeID: nodeIDEff, displayName: *name, region: *region,
		direct: *direct, stun: *stun, turn: *turn, turnUser: *turnUser, turnPass: *turnPass,
		directMode:   *directMode,
		iceKeepalive: *iceKeepalive,
		rateBytes:    *rateBytes, reportInterval: *reportInterval, ctx: ctx,
		sessions: map[uint64]*exitSession{}, byIP: map[netip.Addr]uint64{},
	}
	// Direct-path TLS server config: mTLS (node cert) when certs are provided, else self-signed.
	if *tlsCA != "" && *tlsCert != "" && *tlsKey != "" {
		e.directServerTLS, err = quicx.ServerMTLSFromFiles(*tlsCA, *tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("direct mTLS: %v", err)
		}
		log.Printf("direct-path mTLS enabled")
	} else {
		e.directServerTLS, err = quicx.ServerTLS()
		if err != nil {
			log.Fatalf("direct tls: %v", err)
		}
	}

	// TUN -> session pump is connection-independent (each session carries its own path), so it runs
	// once and survives broker reconnects.
	go e.tunToDatagram()

	// Local status web UI (no auth — bind to localhost). Shows connected clients, path mode, latency,
	// volume, and speed. A per-session throughput sampler feeds it.
	go e.sampleThroughput()
	if sa := strings.TrimSpace(*statusAddr); sa != "" && sa != "off" {
		go func() {
			if err := e.serveStatus(sa); err != nil {
				log.Printf("status web UI: %v", err)
			}
		}()
	}

	// Periodic exit host-utilization reporter (CPU/mem/disk) for the admin dashboard. Survives
	// reconnects; each write targets whichever broker connection is current.
	if e.reportInterval > 0 {
		go func() {
			s := &sysstat.Sampler{}
			s.Sample() // prime CPU baseline
			t := time.NewTicker(e.reportInterval)
			defer t.Stop()
			for {
				select {
				case <-e.ctx.Done():
					return
				case <-t.C:
					st := s.Sample()
					if cw := e.curCW(); cw != nil {
						_ = cw.write(&proto.Control{Type: proto.MsgNodeStatus, CPUPct: st.CPUPct, MemPct: st.MemPct, DiskPct: st.DiskPct})
					}
				}
			}
		}()
	}

	// Graceful shutdown UX: SIGTERM exits immediately; Ctrl-C/Ctrl-Z require a confirming second
	// press. Closing the current broker connection makes the broker mark this exit offline.
	shutdown.OnSignals(3*time.Second, log.Printf, func() {
		log.Printf("shutting down: closing broker connection")
		if c := e.curConn(); c != nil {
			_ = c.CloseWithError(0, "exit shutting down")
		}
		os.Exit(0)
	})

	// Reconnect loop: (re)dial + register, serve until the broker connection drops, then retry with
	// capped backoff. The data plane above is untouched across reconnects.
	backoff := time.Second
	for {
		conn, ctrl, err := dialRegister(ctx, *brokerAddr, brokerTLS, nodeIDEff, *name, *region, *token, *capacity)
		if err != nil {
			log.Printf("cannot reach broker: %s — retrying in %s", quicx.Explain(err, *brokerAddr), backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
			continue
		}
		backoff = time.Second
		if *name != "" {
			log.Printf("registered as %q (%s) region=%s", *name, nodeIDEff, *region)
		} else {
			log.Printf("registered as %s region=%s", nodeIDEff, *region)
		}
		e.serve(conn, ctrl)
		log.Printf("broker connection lost — reconnecting")
	}
}

// deriveNodeID picks this exit's id: an explicit -nodeId wins; otherwise it is derived from -name
// (slugified + a random suffix); with neither, a random "exit-<n>" id is used.
func deriveNodeID(nodeID, name string) string {
	if s := strings.TrimSpace(nodeID); s != "" {
		return s
	}
	if slug := slugify(name); slug != "" {
		return slug + "-" + randSuffix()
	}
	return "exit-" + randSuffix()
}

// slugify lowercases name and replaces any run of non-alphanumeric characters with a single hyphen,
// trimming leading/trailing hyphens. Returns "" if nothing usable remains.
func slugify(name string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// randSuffix returns a 4-digit random number as a string (1000..9999).
func randSuffix() string {
	return strconv.Itoa(1000 + rand.Intn(9000))
}

// dialRegister opens a broker QUIC connection + control stream and registers this exit. The caller
// retries with backoff on error, so transient broker outages don't kill the exit.
func dialRegister(ctx context.Context, brokerAddr string, tlsConf *tls.Config, nodeID, name, region, token string, capacity int) (quic.Connection, quic.Stream, error) {
	conn, err := quic.DialAddr(ctx, brokerAddr, tlsConf, quicx.Config())
	if err != nil {
		return nil, nil, err
	}
	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, nil, err
	}
	if err := proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgRegister, NodeID: nodeID, Name: name, Region: region, Token: token, Capacity: capacity, OS: runtime.GOOS + "/" + runtime.GOARCH}); err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, nil, err
	}
	if _, err := proto.ReadControl(ctrl); err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, nil, err
	}
	return conn, ctrl, nil
}

// serve binds the exit to one broker connection, runs the control + relay-inbound loops until the
// connection drops, then tears down all sessions tied to it (cancelling their per-session
// goroutines via connCtx). Returns when the connection is gone so the caller can reconnect.
func (e *exit) serve(conn quic.Connection, ctrl quic.Stream) {
	cctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.mu.Lock()
	e.conn = conn
	e.cw = &ctrlWriter{s: ctrl}
	e.connCtx = cctx
	e.sessions = map[uint64]*exitSession{}
	e.byIP = map[netip.Addr]uint64{}
	e.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() { e.controlLoop(ctrl); done <- struct{}{} }()
	go func() { e.datagramToTun(cctx, conn); done <- struct{}{} }()
	<-done // either loop returning means the broker connection is dead
	cancel()
	_ = conn.CloseWithError(0, "")
	// Drop sessions bound to this dead connection so the TUN pump stops sending on stale paths.
	e.mu.Lock()
	e.sessions = map[uint64]*exitSession{}
	e.byIP = map[netip.Addr]uint64{}
	e.mu.Unlock()
}

// curConn / curCW return the current broker connection and control writer under lock (they are
// swapped on each reconnect).
func (e *exit) curConn() quic.Connection { e.mu.RLock(); defer e.mu.RUnlock(); return e.conn }
func (e *exit) curCW() *ctrlWriter       { e.mu.RLock(); defer e.mu.RUnlock(); return e.cw }

func (e *exit) controlLoop(ctrl quic.Stream) {
	for {
		msg, err := proto.ReadControl(ctrl)
		if err != nil {
			log.Printf("control closed: %v", err)
			return
		}
		switch msg.Type {
		case proto.MsgSessionStart:
			e.addSession(msg)
		case proto.MsgSessionSuspend:
			e.mu.Lock()
			if es := e.sessions[msg.SessionID]; es != nil {
				es.suspended = true
				es.suspendedAt = time.Now()
			}
			e.mu.Unlock()
		case proto.MsgSessionEnd:
			e.removeSession(msg.SessionID)
		case proto.MsgPong:
			e.mu.RLock()
			es := e.sessions[msg.SessionID]
			e.mu.RUnlock()
			if es != nil {
				es.rtt.Observe(msg.TS)
			}
		case proto.MsgSignal:
			e.mu.RLock()
			es := e.sessions[msg.SessionID]
			e.mu.RUnlock()
			if es != nil {
				var s ice.Signal
				if json.Unmarshal(msg.Signal, &s) == nil {
					select {
					case es.incoming <- &s:
					default:
					}
				}
			}
		}
	}
}

func (e *exit) addSession(msg *proto.Control) {
	sid, clientIP := msg.SessionID, msg.ClientIP
	ip, err := netip.ParseAddr(clientIP)
	if err != nil {
		return
	}
	// Resume: if we still hold this session (it was parked/suspended), just reactivate it and keep
	// its NAT state + counters rather than creating a duplicate.
	e.mu.Lock()
	if es, ok := e.sessions[sid]; ok {
		es.suspended = false
		e.mu.Unlock()
		log.Printf("session %d resumed (client %s reconnected)", sid, clientIP)
		return
	}
	e.mu.Unlock()
	sess := session.New(sid)
	e.mu.RLock()
	conn, cw, connCtx := e.conn, e.cw, e.connCtx
	e.mu.RUnlock()
	if err := sess.StartRelay(session.Path{Sender: conn, Encode: relayEncode(sid)}); err != nil {
		log.Printf("session %d start relay: %v", sid, err)
		return
	}
	if e.rateBytes > 0 {
		sess.SetRateLimit(e.rateBytes, e.rateBytes*2)
	}
	es := &exitSession{
		sess: sess, incoming: make(chan *ice.Signal, 64), clientIP: clientIP,
		stun: msg.StunURL, turn: msg.TurnURL, turnUser: msg.TurnUser, turnPass: msg.TurnPass,
		rtt: &telemetry.RTT{}, startedAt: time.Now(),
	}
	e.mu.Lock()
	e.sessions[sid] = es
	e.byIP[ip] = sid
	e.mu.Unlock()
	log.Printf("session %d started for client %s", sid, clientIP)

	if e.reportInterval > 0 {
		host, _ := os.Hostname()
		meta := telemetry.Meta{
			Host: host, TunName: e.tun.Name(),
			OS:        runtime.GOOS + "/" + runtime.GOARCH,
			UsingStun: es.stun != "", UsingTurn: es.turn != "",
			Downlink: true, // exit egress = the user's download direction
		}
		go telemetry.Run(connCtx, cw, sid, e.reportInterval, sess, meta, es.rtt)
	}
	if e.direct {
		go e.runDirect(connCtx, sid, es)
	}
}

// removeSession tears down a client session on the exit when the broker reports the client
// disconnected (MsgSessionEnd): it drops the session from the maps so the status page count and the
// TUN routing stop referencing it. The session's direct goroutine (if any) ends when its QUIC conn
// errors shortly after the client leaves.
func (e *exit) removeSession(sid uint64) {
	e.mu.Lock()
	es := e.sessions[sid]
	delete(e.sessions, sid)
	if es != nil {
		if ip, err := netip.ParseAddr(es.clientIP); err == nil {
			delete(e.byIP, ip)
		}
	}
	e.mu.Unlock()
	if es != nil {
		log.Printf("session %d ended (client %s disconnected)", sid, es.clientIP)
	}
}

// datagramToTun handles relay inbound (broker datagrams, sid-prefixed) -> shared TUN, with
// per-session anti-spoof (source IP must match the session's assigned VPN IP).
func (e *exit) datagramToTun(ctx context.Context, conn quic.Connection) {
	for {
		dg, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			log.Printf("relay recv: %v", err)
			return
		}
		sid, pkt, err := proto.DecodeDatagram(dg)
		if err != nil {
			continue
		}
		e.mu.RLock()
		es := e.sessions[sid]
		e.mu.RUnlock()
		if es != nil {
			e.writeInbound(es, pkt)
		}
	}
}

// tunToDatagram reads reply packets from the shared TUN, finds the owning session by destination
// (client) IP, and sends over that session's active path (relay or direct).
func (e *exit) tunToDatagram() {
	buf := make([]byte, 65535)
	for {
		n, err := e.tun.Read(buf)
		if err != nil {
			log.Printf("tun read: %v", err)
			return
		}
		dst, ok := ipv4Dst(buf[:n])
		if !ok {
			continue
		}
		e.mu.RLock()
		sid, found := e.byIP[dst]
		var es *exitSession
		if found {
			es = e.sessions[sid]
		}
		e.mu.RUnlock()
		if es != nil {
			if err := es.sess.Send(buf[:n]); err != nil && err != session.ErrNoPath && err != session.ErrRateLimited {
				log.Printf("session %d send: %v", sid, err)
			}
		}
	}
}

// runDirect negotiates the direct ICE/QUIC path (controlled) for a session and migrates onto it.
func (e *exit) runDirect(ctx context.Context, sid uint64, es *exitSession) {
	stunV, turnV := orStr(es.stun, e.stun), orStr(es.turn, e.turn)
	userV, passV := orStr(es.turnUser, e.turnUser), orStr(es.turnPass, e.turnPass)
	agent, err := ice.NewPionAgent(ice.PionConfig{
		Role: ice.RoleControlled, STUNURLs: splitCSV(stunV), TURNURLs: splitCSV(turnV),
		TURNUser: userV, TURNPass: passV, KeepaliveInterval: e.iceKeepalive,
	})
	if err != nil {
		log.Printf("session %d direct agent: %v", sid, err)
		return
	}
	defer agent.Close()

	send := func(s *ice.Signal) error {
		b, _ := json.Marshal(s)
		return e.curCW().write(&proto.Control{Type: proto.MsgSignal, SessionID: sid, Signal: b})
	}
	serverTLS := e.directServerTLS
	if err := es.sess.BeginChecks(); err != nil {
		log.Printf("session %d begin checks: %v", sid, err)
		return
	}
	qc, err := icewire.Negotiate(ctx, agent, ice.RoleControlled, fmt.Sprintf("%d", sid), send, es.incoming, serverTLS, e.directMode == "p2p-only")
	if err != nil {
		if errors.Is(err, directlink.ErrRelayPair) {
			log.Printf("session %d: only a TURN-relayed path is available — staying on relay (-direct-mode p2p-only)", sid)
		} else {
			log.Printf("session %d direct negotiate failed, staying on relay: %v", sid, err)
		}
		_ = es.sess.ChecksFailed()
		return
	}
	if err := es.sess.UpgradeDirect(session.Path{Sender: qc, Encode: session.Identity}); err != nil {
		log.Printf("session %d upgrade: %v", sid, err)
		return
	}
	log.Printf("session %d: upgraded to DIRECT path", sid)
	for {
		dg, err := qc.ReceiveDatagram(ctx)
		if err != nil {
			// If the broker connection (this session) is gone, just stop — the session is being
			// torn down and will be re-established on reconnect.
			if ctx.Err() != nil {
				return
			}
			// Direct path died — fall back to the relay so this session keeps flowing.
			if ferr := es.sess.FallbackRelay(session.Path{Sender: e.curConn(), Encode: relayEncode(sid)}); ferr != nil {
				log.Printf("session %d direct recv: %v; relay fallback failed: %v", sid, err, ferr)
			} else {
				log.Printf("session %d direct recv: %v; fell back to relay", sid, err)
			}
			return
		}
		e.writeInbound(es, dg)
	}
}

func relayEncode(sid uint64) func([]byte) []byte {
	return func(pkt []byte) []byte { return proto.EncodeDatagram(sid, pkt) }
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// orStr returns a if non-empty, else b.
func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ipv4Dst extracts the destination address from an IPv4 packet.
func ipv4Dst(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{pkt[16], pkt[17], pkt[18], pkt[19]}), true
}

// ipv4Src extracts the source address from an IPv4 packet.
func ipv4Src(pkt []byte) (netip.Addr, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{pkt[12], pkt[13], pkt[14], pkt[15]}), true
}

// writeInbound writes a client packet to the TUN only if its source IP matches the session's
// assigned VPN IP (anti-spoofing — a client cannot inject packets as another tenant).
func (e *exit) writeInbound(es *exitSession, pkt []byte) {
	src, ok := ipv4Src(pkt)
	if !ok || src.String() != es.clientIP {
		return // drop non-IPv4 or spoofed source
	}
	es.bytesUp.Add(uint64(len(pkt)))
	_, _ = e.tun.Write(pkt)
}
