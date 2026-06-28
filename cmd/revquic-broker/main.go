// SPDX-License-Identifier: GPL-3.0-or-later

// Command revquic-broker is the Revquic broker (B).
//
// Phase 0: accepts QUIC connections from exit nodes (C) and clients (A), pairs a client to an
// exit by region, and relays QUIC datagrams (IP packets) between them by session id.
//
// Phase 1 (this build) adds: node shared-secret + client token auth, per-user region
// enforcement (userstore), an in-process event bus, and the admin API/SSE server
// (spec/api/admin-openapi.yaml). mTLS + OIDC remain the production upgrade (see spec/PHASE1.md).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/adminapi"
	"github.com/sourjatilak/revquic/internal/adminserver"
	"github.com/sourjatilak/revquic/internal/adminstore"
	"github.com/sourjatilak/revquic/internal/auth"
	"github.com/sourjatilak/revquic/internal/conf"
	"github.com/sourjatilak/revquic/internal/events"
	"github.com/sourjatilak/revquic/internal/ippool"
	"github.com/sourjatilak/revquic/internal/lb"
	"github.com/sourjatilak/revquic/internal/logx"
	"github.com/sourjatilak/revquic/internal/oidc"
	"github.com/sourjatilak/revquic/internal/proto"
	"github.com/sourjatilak/revquic/internal/qos"
	"github.com/sourjatilak/revquic/internal/quicx"
	"github.com/sourjatilak/revquic/internal/turncred"
	"github.com/sourjatilak/revquic/internal/userstore"
)

type exitNode struct {
	nodeID      string
	name        string
	region      string
	conn        quic.Connection
	ctrl        quic.Stream
	cw          *ctrlWriter
	capacity    int
	connectedAt time.Time
	osInfo      string
	active      atomic.Int64
	cpuBits     atomic.Uint64 // host CPU%  as float64 bits (MsgNodeStatus)
	memBits     atomic.Uint64 // host mem%  as float64 bits
	diskBits    atomic.Uint64 // host disk% as float64 bits
}

func (e *exitNode) setSysStat(cpu, mem, disk float64) {
	e.cpuBits.Store(math.Float64bits(cpu))
	e.memBits.Store(math.Float64bits(mem))
	e.diskBits.Store(math.Float64bits(disk))
}

func (e *exitNode) sysStat() (cpu, mem, disk float64) {
	return math.Float64frombits(e.cpuBits.Load()),
		math.Float64frombits(e.memBits.Load()),
		math.Float64frombits(e.diskBits.Load())
}

// ctrlWriter serializes writes to a control stream (multiple goroutines may forward signals to it).
type ctrlWriter struct {
	mu sync.Mutex
	s  quic.Stream
}

func (c *ctrlWriter) write(m *proto.Control) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return proto.WriteControl(c.s, m)
}

type session struct {
	id           uint64
	region       string
	clientIP     string
	clientAddr   netip.Addr
	username     string
	userID       string
	name         string
	resumeKey    string
	suspended    bool
	parkedAt     time.Time
	suspendTimer *time.Timer
	nodeID       string
	startedAt    time.Time
	clientConn   quic.Connection
	exitConn     quic.Connection
	clientCW     *ctrlWriter
	exitCW       *ctrlWriter
}

type broker struct {
	mu       sync.RWMutex
	exits    map[string]*exitNode
	sessions map[uint64]*session
	parked   map[string]*session // resumeKey -> suspended (resumable) session

	resumeTTL time.Duration // how long a disconnected client's session stays resumable

	nextSession atomic.Uint64
	ipPool      *ippool.Pool

	users        userstore.Store
	bus          *events.Bus
	nodeSecret   string
	oidcVerifier *oidc.Verifier
	turnSecret   string
	turnURL      string
	stunURL      string

	qos        *qos.Tracker
	lbStrategy lb.Strategy
	rr         uint64 // round-robin cursor (atomic via mu held in handleClient)
}

func newBroker(users userstore.Store, bus *events.Bus, nodeSecret string) *broker {
	b := &broker{
		exits:      map[string]*exitNode{},
		sessions:   map[uint64]*session{},
		parked:     map[string]*session{},
		resumeTTL:  time.Hour,
		users:      users,
		bus:        bus,
		nodeSecret: nodeSecret,
		qos:        qos.New(qos.DefaultHistory),
		lbStrategy: lb.LeastConn,
	}
	b.ipPool = mustPool("10.99.0.0/24", "10.99.0.1")
	return b
}

func mustPool(cidr, gateway string) *ippool.Pool {
	gw, _ := netip.ParseAddr(gateway)
	p, err := ippool.New(cidr, gw)
	if err != nil {
		panic(err)
	}
	return p
}

func main() {
	quicAddr := flag.String("quic", ":4242", "QUIC listen address (UDP)")
	configPath := flag.String("config", "", "path to a key=value config file (CLI flags take precedence)")
	logFile := flag.String("log-file", "", "write logs to this file (default: stderr)")
	logType := flag.String("log-type", "text", "log format: text | json")
	httpAddr := flag.String("http", ":8080", "admin HTTP listen address")
	httpTLSCert := flag.String("http-tls-cert", "", "TLS cert PEM for the admin web server; with -http-tls-key serves HTTPS")
	httpTLSKey := flag.String("http-tls-key", "", "TLS key PEM for the admin web server (HTTPS)")
	adminToken := flag.String("admin-token", "admin-secret", "bootstrap bearer token accepted by the admin API (in addition to login sessions)")
	adminUser := flag.String("admin-user", "admin", "seed admin account username")
	adminPass := flag.String("admin-pass", "admin", "seed admin account password (PBKDF2-hashed at rest)")
	admindb := flag.String("admindb", "", "path to persist admin accounts (JSON); empty = in-memory")
	nodeToken := flag.String("node-token", "node-secret", "shared secret exit nodes must present")
	lbStrategy := flag.String("lb", "least-conn", "exit load-balancing strategy: least-conn | round-robin | random")
	resumeTTL := flag.Duration("session-resume-ttl", time.Hour, "how long a disconnected client's session stays resumable (same exit + VPN IP) before it is fully ended; 0 disables resume")
	userdb := flag.String("userdb", "", "path to persist users (JSON); empty = in-memory only")
	qosdb := flag.String("qosdb", "", "path to persist QoS event history (file/sqlite store); empty = store default")
	credPepper := flag.String("cred-pepper", "dev-pepper", "server pepper for hashing client tokens (keep stable & secret)")
	store := flag.String("store", "mem", "user/admin store backend: mem | file | sqlite (paths from -userdb/-admindb)")
	seedUser := flag.String("seed-user", "", "seed a client user as token:region[,region] (spike convenience)")
	seedOIDCUser := flag.String("seed-oidc-user", "", "seed an OIDC user as <username/email>:region[,region] (no token; authenticated via OIDC)")
	tlsCA := flag.String("tls-ca", "", "CA cert PEM; if set with -tls-cert/-tls-key, enables mTLS on the QUIC listener")
	tlsCert := flag.String("tls-cert", "", "server leaf cert PEM (mTLS)")
	tlsKey := flag.String("tls-key", "", "server leaf key PEM (mTLS)")
	oidcIssuer := flag.String("oidc-issuer", "", "OIDC issuer; if set with -oidc-audience and a JWKS source, client tokens are verified as ID tokens")
	oidcAudience := flag.String("oidc-audience", "", "OIDC audience (client_id)")
	oidcJWKSURL := flag.String("oidc-jwks-url", "", "OIDC JWKS URL (jwks_uri)")
	oidcJWKSFile := flag.String("oidc-jwks-file", "", "OIDC JWKS file (alternative to -oidc-jwks-url)")
	turnSecret := flag.String("turn-secret", "", "coturn static-auth-secret; if set, the broker mints per-session TURN REST creds")
	turnURL := flag.String("turn-url", "", "TURN URL advertised to peers, e.g. turn:coturn:3478")
	stunURL := flag.String("stun-url", "", "STUN URL advertised to peers, e.g. stun:coturn:3478")
	flag.Parse()
	if *configPath != "" {
		if err := conf.ApplyFile(flag.CommandLine, *configPath); err != nil {
			log.Fatalf("config: %v", err)
		}
	}
	logClose, lerr := logx.Setup("broker", *logFile, *logType)
	if lerr != nil {
		log.Fatalf("log: %v", lerr)
	}
	defer logClose()

	if *credPepper == "dev-pepper" {
		log.Printf("WARNING: using default -cred-pepper; set a stable secret pepper in production")
	}
	var users userstore.Store
	switch *store {
	case "sqlite":
		path := *userdb
		if path == "" {
			path = "users.db"
		}
		s, err := userstore.NewSQLite(path, *credPepper)
		if err != nil {
			log.Fatalf("open sqlite userdb %s: %v", path, err)
		}
		users = s
		log.Printf("user store: sqlite %s", path)
	case "file":
		if *userdb == "" {
			log.Fatalf("-store=file requires -userdb")
		}
		s, err := userstore.NewFile(*userdb, *credPepper)
		if err != nil {
			log.Fatalf("open userdb %s: %v", *userdb, err)
		}
		users = s
		log.Printf("user store: file %s", *userdb)
	default:
		users = userstore.New(*credPepper)
		log.Printf("user store: in-memory (non-persistent)")
	}
	if *seedUser != "" {
		tok, regions, ok := strings.Cut(*seedUser, ":")
		if ok {
			if _, err := users.Create(adminapi.UserCreate{
				Username:       "seed",
				Credential:     tok,
				AllowedRegions: strings.Split(regions, ","),
				Status:         adminapi.UserEnabled,
			}); err != nil && err != userstore.ErrConflict {
				log.Fatalf("seed user: %v", err)
			}
			log.Printf("seeded client user token=%q regions=%q", tok, regions)
		}
	}
	if *seedOIDCUser != "" {
		uname, regions, ok := strings.Cut(*seedOIDCUser, ":")
		if ok {
			if _, err := users.Create(adminapi.UserCreate{
				Username:       uname,
				AllowedRegions: strings.Split(regions, ","),
				Status:         adminapi.UserEnabled,
			}); err != nil && err != userstore.ErrConflict {
				log.Fatalf("seed oidc user: %v", err)
			}
			log.Printf("seeded OIDC user %q regions=%q", uname, regions)
		}
	}

	bus := events.NewBus()
	b := newBroker(users, bus, *nodeToken)
	b.lbStrategy = lb.Parse(*lbStrategy)
	log.Printf("exit load-balancing strategy: %s", b.lbStrategy)
	b.resumeTTL = *resumeTTL
	if b.resumeTTL > 0 {
		log.Printf("session resume window: %s", b.resumeTTL)
	}

	// Persist QoS event history when a durable store is selected (survives broker restarts).
	switch *store {
	case "sqlite":
		path := *qosdb
		if path == "" {
			path = "qos.db"
		}
		hs, err := qos.NewSQLiteHistory(path)
		if err != nil {
			log.Fatalf("open sqlite qos history %s: %v", path, err)
		}
		b.qos.WithStore(hs)
		if err := b.qos.Preload(qos.DefaultHistory); err != nil {
			log.Printf("qos history preload: %v", err)
		}
		log.Printf("QoS history: sqlite %s", path)
	case "file":
		path := *qosdb
		if path == "" {
			path = "qos-history.jsonl"
		}
		hs, err := qos.NewFileHistory(path)
		if err != nil {
			log.Fatalf("open file qos history %s: %v", path, err)
		}
		b.qos.WithStore(hs)
		if err := b.qos.Preload(qos.DefaultHistory); err != nil {
			log.Printf("qos history preload: %v", err)
		}
		log.Printf("QoS history: file %s", path)
	default:
		log.Printf("QoS history: in-memory (non-persistent)")
	}

	b.turnSecret, b.turnURL, b.stunURL = *turnSecret, *turnURL, *stunURL
	if *turnSecret != "" {
		log.Printf("TURN REST creds enabled (turn=%s stun=%s)", *turnURL, *stunURL)
	}

	if *oidcIssuer != "" && *oidcAudience != "" {
		switch {
		case *oidcJWKSURL != "":
			b.oidcVerifier = oidc.NewURLVerifier(*oidcIssuer, *oidcAudience, *oidcJWKSURL)
			log.Printf("OIDC user auth enabled (issuer=%s, jwks=%s)", *oidcIssuer, *oidcJWKSURL)
		case *oidcJWKSFile != "":
			jwksJSON, rerr := os.ReadFile(*oidcJWKSFile)
			if rerr != nil {
				log.Fatalf("oidc jwks file: %v", rerr)
			}
			v, verr := oidc.NewStaticVerifier(*oidcIssuer, *oidcAudience, jwksJSON)
			if verr != nil {
				log.Fatalf("oidc verifier: %v", verr)
			}
			b.oidcVerifier = v
			log.Printf("OIDC user auth enabled (issuer=%s, jwks-file=%s)", *oidcIssuer, *oidcJWKSFile)
		default:
			log.Fatalf("oidc: set -oidc-jwks-url or -oidc-jwks-file")
		}
	}

	// admin accounts (passwords PBKDF2-hashed at rest)
	var admins adminstore.Store
	switch *store {
	case "sqlite":
		path := *admindb
		if path == "" {
			path = "admins.db"
		}
		s, err := adminstore.NewSQLite(path)
		if err != nil {
			log.Fatalf("open sqlite admindb %s: %v", path, err)
		}
		admins = s
		log.Printf("admin store: sqlite %s", path)
	case "file":
		if *admindb == "" {
			log.Fatalf("-store=file requires -admindb")
		}
		s, err := adminstore.NewFile(*admindb)
		if err != nil {
			log.Fatalf("open admindb %s: %v", *admindb, err)
		}
		admins = s
		log.Printf("admin store: file %s", *admindb)
	default:
		admins = adminstore.New()
		log.Printf("admin store: in-memory (non-persistent)")
	}
	if len(admins.List()) == 0 && *adminUser != "" {
		if _, err := admins.Create(*adminUser, *adminPass, adminstore.RoleAdmin); err != nil {
			log.Fatalf("seed admin: %v", err)
		}
		log.Printf("seeded admin account %q", *adminUser)
	}

	srv := &adminserver.Server{
		Users:          users,
		Admins:         admins,
		Bus:            bus,
		Nodes:          b,
		QoS:            b,
		BootstrapToken: *adminToken,
	}
	go func() {
		if *httpTLSCert != "" && *httpTLSKey != "" {
			log.Printf("admin HTTPS on %s (cert %s)", *httpAddr, *httpTLSCert)
			if err := http.ListenAndServeTLS(*httpAddr, *httpTLSCert, *httpTLSKey, srv.Handler()); err != nil {
				log.Printf("https: %v", err)
			}
			return
		}
		if *httpTLSCert != "" || *httpTLSKey != "" {
			log.Printf("WARNING: -http-tls-cert and -http-tls-key must both be set for HTTPS; serving plain HTTP")
		}
		log.Printf("admin HTTP on %s", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, srv.Handler()); err != nil {
			log.Printf("http: %v", err)
		}
	}()

	var (
		tlsConf *tls.Config
		err     error
	)
	if *tlsCA != "" && *tlsCert != "" && *tlsKey != "" {
		tlsConf, err = quicx.ServerMTLSFromFiles(*tlsCA, *tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("mTLS: %v", err)
		}
		log.Printf("control-plane mTLS enabled (CA %s)", *tlsCA)
	} else {
		tlsConf, err = quicx.ServerTLS()
		if err != nil {
			log.Fatalf("tls: %v", err)
		}
		log.Printf("control-plane TLS: self-signed (no client-cert verification)")
	}
	ln, err := quic.ListenAddr(*quicAddr, tlsConf, quicx.Config())
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("broker QUIC listening on %s", *quicAddr)

	// Graceful shutdown: flush the persistent QoS history (drains the async writer) on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("shutdown (%s): flushing QoS history", sig)
		if err := b.qos.CloseStore(); err != nil {
			log.Printf("qos store close: %v", err)
		}
		_ = ln.Close()
		os.Exit(0)
	}()

	ctx := context.Background()
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go b.handleConn(ctx, conn)
	}
}

func (b *broker) handleConn(ctx context.Context, conn quic.Connection) {
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	msg, err := proto.ReadControl(stream)
	if err != nil {
		return
	}
	switch msg.Type {
	case proto.MsgRegister:
		b.handleExit(ctx, conn, stream, msg)
	case proto.MsgConnect:
		b.handleClient(ctx, conn, stream, msg)
	default:
		_ = proto.WriteControl(stream, &proto.Control{Type: proto.MsgError, Error: "unexpected first message"})
	}
}

func (b *broker) handleExit(ctx context.Context, conn quic.Connection, ctrl quic.Stream, msg *proto.Control) {
	if !auth.ConstantEqual(msg.Token, b.nodeSecret) {
		_ = proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgError, Error: "bad node token"})
		return
	}
	ex := &exitNode{nodeID: msg.NodeID, name: msg.Name, region: msg.Region, conn: conn, ctrl: ctrl, cw: &ctrlWriter{s: ctrl}, capacity: msg.Capacity, connectedAt: time.Now().UTC(), osInfo: msg.OS}
	if ex.capacity == 0 {
		ex.capacity = 100
	}
	b.mu.Lock()
	b.exits[ex.nodeID] = ex
	b.mu.Unlock()
	if ex.name != "" {
		log.Printf("exit registered: %q (%s) region=%s", ex.name, ex.nodeID, ex.region)
	} else {
		log.Printf("exit registered: %s region=%s", ex.nodeID, ex.region)
	}
	_ = ex.cw.write(&proto.Control{Type: proto.MsgRegisterOK})
	b.qos.NodeUp(ex.nodeID, ex.region, ex.connectedAt)
	b.bus.Publish(adminapi.Event{Type: adminapi.EvNodeConnected, TS: time.Now().UTC(), Node: b.deviceView(ex)})

	go b.relayLoop(ctx, conn)

	// Control read loop: forward ICE signaling from this exit to the matching client,
	// and ingest per-session QoS reports.
	for {
		m, err := proto.ReadControl(ctrl)
		if err != nil {
			break
		}
		switch m.Type {
		case proto.MsgSignal:
			b.mu.RLock()
			s := b.sessions[m.SessionID]
			b.mu.RUnlock()
			if s != nil && s.clientCW != nil {
				_ = s.clientCW.write(m)
			}
		case proto.MsgReport:
			b.ingestReport(m)
		case proto.MsgPing:
			_ = ex.cw.write(&proto.Control{Type: proto.MsgPong, SessionID: m.SessionID, TS: m.TS})
		case proto.MsgNodeStatus:
			ex.setSysStat(m.CPUPct, m.MemPct, m.DiskPct)
			b.bus.Publish(adminapi.Event{Type: adminapi.EvNodeUpdated, TS: time.Now().UTC(), Node: b.deviceView(ex)})
		}
	}
	b.mu.Lock()
	delete(b.exits, ex.nodeID)
	b.mu.Unlock()
	log.Printf("exit gone: %s", ex.nodeID)
	b.qos.NodeDown(ex.nodeID)
	b.bus.Publish(adminapi.Event{Type: adminapi.EvNodeDisconnected, TS: time.Now().UTC(), NodeID: ex.nodeID})
	b.failExit(ex)
}

// failExit notifies and disconnects every client session bound to a now-gone exit, so the client
// retries against a healthy exit instead of silently black-holing traffic on a dead path. It sends
// MsgError (best-effort) then closes the client connection; the normal teardown (endSession + QoS +
// event) runs when that connection's relay loop unblocks.
func (b *broker) failExit(ex *exitNode) {
	b.mu.RLock()
	var affected []*session
	for _, s := range b.sessions {
		if s.nodeID == ex.nodeID {
			affected = append(affected, s)
		}
	}
	b.mu.RUnlock()
	if len(affected) == 0 {
		return
	}
	log.Printf("exit %s down: disconnecting %d client session(s)", ex.nodeID, len(affected))
	for _, s := range affected {
		if s.clientCW != nil {
			_ = s.clientCW.write(&proto.Control{Type: proto.MsgError, Error: "exit node " + ex.nodeID + " disconnected; please reconnect"})
		}
		if s.clientConn != nil {
			_ = s.clientConn.CloseWithError(0, "exit node disconnected")
		}
	}
}

func (b *broker) handleClient(ctx context.Context, conn quic.Connection, ctrl quic.Stream, msg *proto.Control) {
	var user adminapi.User
	var err error
	if b.oidcVerifier != nil {
		claims, verr := b.oidcVerifier.Verify(ctx, msg.Token)
		if verr != nil {
			_ = proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgError, Error: "oidc: " + verr.Error()})
			return
		}
		user, err = b.users.AuthorizeUser(claims.Identity(), msg.RequestedRegion)
	} else {
		user, err = b.users.AuthenticateForRegion(msg.Token, msg.RequestedRegion)
	}
	if err != nil {
		_ = proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgError, Error: err.Error()})
		return
	}

	b.mu.Lock()
	// List-exits request: reply with the region's available exits and return (no session).
	if msg.ListExits {
		var list []proto.ExitInfo
		for _, e := range b.exits {
			if e.region == msg.RequestedRegion {
				list = append(list, proto.ExitInfo{NodeID: e.nodeID, Name: e.name, Region: e.region, System: e.osInfo, ActiveUsers: int(e.active.Load()), Capacity: e.capacity})
			}
		}
		b.mu.Unlock()
		_ = proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgConnectOK, ExitList: list})
		return
	}
	clientCW := &ctrlWriter{s: ctrl}

	// Resume: if the client supplied a resume key and we have a parked (recently disconnected)
	// session for it whose exit is still online, reattach to that same session (same exit + VPN IP)
	// instead of allocating a new one.
	if msg.ResumeKey != "" {
		if s, ex := b.tryResume(msg.ResumeKey, user.Username, conn, clientCW); s != nil {
			b.runSession(ctx, conn, ctrl, clientCW, s, ex, true)
			return
		}
	}

	var ex *exitNode
	if msg.RequestedExit != "" {
		// Manual selection: use the named exit if it serves the region and has capacity.
		if e := b.exits[msg.RequestedExit]; e != nil && e.region == msg.RequestedRegion &&
			(e.capacity == 0 || int(e.active.Load()) < e.capacity) {
			ex = e
		}
		b.mu.Unlock()
		if ex == nil {
			_ = proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgError, Error: "requested exit " + msg.RequestedExit + " unavailable in region " + msg.RequestedRegion})
			return
		}
	} else {
		// Auto: load-balance across the region's exits.
		cands := make([]lb.Node, 0, len(b.exits))
		for _, e := range b.exits {
			cands = append(cands, lb.Node{ID: e.nodeID, Region: e.region, Active: int(e.active.Load()), Capacity: e.capacity})
		}
		chosen, ok := lb.Pick(b.lbStrategy, msg.RequestedRegion, cands, &b.rr)
		if ok {
			ex = b.exits[chosen]
		}
		b.mu.Unlock()
		if ex == nil {
			_ = proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgError, Error: "no exit available in region " + msg.RequestedRegion})
			return
		}
	}

	sid := b.nextSession.Add(1)
	clientAddr, err := b.ipPool.Allocate()
	if err != nil {
		_ = proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgError, Error: "exit address pool exhausted"})
		return
	}
	clientIP := clientAddr.String()
	s := &session{
		id: sid, region: ex.region, clientIP: clientIP, clientAddr: clientAddr, username: user.Username, userID: user.ID, name: msg.Name,
		resumeKey: msg.ResumeKey,
		nodeID:    ex.nodeID, startedAt: time.Now().UTC(), clientConn: conn, exitConn: ex.conn,
		clientCW: clientCW, exitCW: ex.cw,
	}
	b.mu.Lock()
	b.sessions[sid] = s
	b.mu.Unlock()
	ex.active.Add(1)
	b.qos.Connect(fmt.Sprintf("%d", sid), ex.nodeID, ex.region, user.Username, s.startedAt)
	b.runSession(ctx, conn, ctrl, clientCW, s, ex, false)
}

// tryResume reattaches a parked session for resumeKey (same user, not expired, exit still online).
// It is called with b.mu held; on success it returns the session+exit with the lock RELEASED and
// the session moved back to active. On failure it returns (nil,nil) with the lock still HELD so the
// caller proceeds to allocate a new session.
func (b *broker) tryResume(resumeKey, username string, conn quic.Connection, clientCW *ctrlWriter) (*session, *exitNode) {
	ps := b.parked[resumeKey]
	if ps == nil || ps.username != username || time.Since(ps.parkedAt) >= b.resumeTTL {
		return nil, nil
	}
	ex := b.exits[ps.nodeID]
	if ex == nil { // the exit it was bound to is offline; can't resume to it
		return nil, nil
	}
	if ps.suspendTimer != nil {
		ps.suspendTimer.Stop()
	}
	delete(b.parked, resumeKey)
	ps.suspended = false
	ps.clientConn = conn
	ps.clientCW = clientCW
	ps.exitConn = ex.conn
	ps.exitCW = ex.cw
	b.sessions[ps.id] = ps
	b.mu.Unlock()
	ex.active.Add(1)
	return ps, ex
}

// runSession sends SessionStart to the exit, ConnectOK to the client, then runs the client control
// loop + relay until the connection drops, at which point the session is suspended (parked) or ended.
func (b *broker) runSession(ctx context.Context, conn quic.Connection, ctrl quic.Stream, clientCW *ctrlWriter, s *session, ex *exitNode, resumed bool) {
	sid := s.id
	var stunURL, turnURL, turnUser, turnPass string
	if b.turnSecret != "" {
		stunURL, turnURL = b.stunURL, b.turnURL
		turnUser, turnPass = turncred.Credentials(b.turnSecret, time.Hour, fmt.Sprintf("%d", sid))
	}
	if err := ex.cw.write(&proto.Control{
		Type: proto.MsgSessionStart, SessionID: sid, ClientIP: s.clientIP, Netmask: "255.255.255.0", MTU: proto.SafeTunnelMTU,
		StunURL: stunURL, TurnURL: turnURL, TurnUser: turnUser, TurnPass: turnPass,
	}); err != nil {
		_ = clientCW.write(&proto.Control{Type: proto.MsgError, Error: "exit unavailable"})
		b.endSession(s, ex)
		return
	}
	_ = clientCW.write(&proto.Control{
		Type: proto.MsgConnectOK, SessionID: sid, ClientIP: s.clientIP, Netmask: "255.255.255.0", MTU: proto.SafeTunnelMTU,
		StunURL: stunURL, TurnURL: turnURL, TurnUser: turnUser, TurnPass: turnPass,
	})
	if resumed {
		log.Printf("session %d resumed: user=%s exit=%s ip=%s", sid, s.username, ex.nodeID, s.clientIP)
	} else {
		log.Printf("session %d: user=%s region=%s exit=%s ip=%s", sid, s.username, ex.region, ex.nodeID, s.clientIP)
	}
	b.bus.Publish(adminapi.Event{Type: adminapi.EvSessionStarted, TS: time.Now().UTC(), Session: b.sessionView(s)})

	// Control read loop: forward ICE signaling from this client to its exit, and ingest QoS reports.
	go func() {
		for {
			m, err := proto.ReadControl(ctrl)
			if err != nil {
				return
			}
			switch m.Type {
			case proto.MsgSignal:
				_ = ex.cw.write(m)
			case proto.MsgReport:
				b.ingestReport(m)
			case proto.MsgPing:
				_ = clientCW.write(&proto.Control{Type: proto.MsgPong, SessionID: m.SessionID, TS: m.TS})
			}
		}
	}()

	cause := b.relayLoop(ctx, conn)
	b.suspendSession(s, ex, clientClosedIntentionally(cause))
}

// clientClosedIntentionally reports whether the client deliberately closed its connection (Ctrl-C /
// SIGTERM), signalled by the proto.CloseClientShutdown QUIC application error code. A transient drop,
// idle timeout, or sleep/wake reconnect does NOT match, so those still park the session for resume.
func clientClosedIntentionally(err error) bool {
	var ae *quic.ApplicationError
	return errors.As(err, &ae) && uint64(ae.ErrorCode) == proto.CloseClientShutdown
}

// suspendSession is called when a client connection drops. If the client supplied a resume key and
// the exit is online, the session is PARKED (kept resumable for resumeTTL: VPN IP stays reserved, the
// exit keeps the session marked suspended). Otherwise the session is fully torn down immediately.
func (b *broker) suspendSession(s *session, ex *exitNode, intentional bool) {
	b.mu.Lock()
	if _, ok := b.sessions[s.id]; !ok {
		b.mu.Unlock() // already resumed elsewhere or torn down
		return
	}
	delete(b.sessions, s.id)
	ex.active.Add(-1)
	if !intentional && s.resumeKey != "" && b.resumeTTL > 0 {
		s.suspended = true
		s.parkedAt = time.Now()
		b.parked[s.resumeKey] = s
		s.suspendTimer = time.AfterFunc(b.resumeTTL, func() { b.expirePark(s.resumeKey, s.id) })
		b.mu.Unlock()
		_ = ex.cw.write(&proto.Control{Type: proto.MsgSessionSuspend, SessionID: s.id})
		log.Printf("session %d suspended: client offline, resumable for %s", s.id, b.resumeTTL)
		return
	}
	b.ipPool.Release(s.clientAddr)
	b.mu.Unlock()
	_ = ex.cw.write(&proto.Control{Type: proto.MsgSessionEnd, SessionID: s.id})
	b.qos.Disconnect(fmt.Sprintf("%d", s.id))
	if intentional {
		log.Printf("session %d closed: client exited", s.id)
	} else {
		log.Printf("session %d closed", s.id)
	}
	b.bus.Publish(adminapi.Event{Type: adminapi.EvSessionEnded, TS: time.Now().UTC(), Session: b.sessionView(s)})
}

// expirePark fully tears down a parked session whose resume window elapsed without a reconnect.
func (b *broker) expirePark(resumeKey string, sid uint64) {
	b.mu.Lock()
	s := b.parked[resumeKey]
	if s == nil || s.id != sid || !s.suspended {
		b.mu.Unlock() // already resumed or ended
		return
	}
	delete(b.parked, resumeKey)
	b.ipPool.Release(s.clientAddr)
	ex := b.exits[s.nodeID]
	b.mu.Unlock()
	if ex != nil {
		_ = ex.cw.write(&proto.Control{Type: proto.MsgSessionEnd, SessionID: sid})
	}
	b.qos.Disconnect(fmt.Sprintf("%d", sid))
	log.Printf("session %d expired: no reconnect within %s", sid, b.resumeTTL)
	b.bus.Publish(adminapi.Event{Type: adminapi.EvSessionEnded, TS: time.Now().UTC(), Session: b.sessionView(s)})
}

// ingestReport feeds a client/exit MsgReport into the QoS tracker.
func (b *broker) ingestReport(m *proto.Control) {
	b.qos.Report(fmt.Sprintf("%d", m.SessionID), qos.Report{
		BytesUp: m.BytesUp, BytesDown: m.BytesDown, ThroughputBps: m.ThroughputBps,
		Drops: m.Drops, RTTms: m.RTTms, Direct: m.Direct, Host: m.Host, TunName: m.TunName, OS: m.OS, CPUPct: m.CPUPct, MemPct: m.MemPct, DiskPct: m.DiskPct,
	})
}

func (b *broker) endSession(s *session, ex *exitNode) {
	b.mu.Lock()
	_, ok := b.sessions[s.id]
	if ok {
		delete(b.sessions, s.id)
		ex.active.Add(-1)
		b.ipPool.Release(s.clientAddr)
	}
	b.mu.Unlock()
	// Tell the exit the client is gone so it removes the session (frees its status/count + goroutines).
	if ok && ex.cw != nil {
		_ = ex.cw.write(&proto.Control{Type: proto.MsgSessionEnd, SessionID: s.id})
	}
}

func (b *broker) relayLoop(ctx context.Context, conn quic.Connection) error {
	for {
		dg, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return err
		}
		sid, pkt, err := proto.DecodeDatagram(dg)
		if err != nil {
			continue
		}
		b.mu.RLock()
		s := b.sessions[sid]
		b.mu.RUnlock()
		if s == nil {
			continue
		}
		// Meter relayed traffic: datagrams arriving on the exit conn flow exit->client ("down");
		// otherwise client->exit ("up").
		fromClient := conn != s.exitConn
		b.qos.AddBytes(fmt.Sprintf("%d", sid), fromClient, len(pkt))
		peer := s.exitConn
		if conn == s.exitConn {
			peer = s.clientConn
		}
		if peer != nil {
			_ = peer.SendDatagram(dg)
		}
	}
}

// --- adminserver.NodeProvider ---

func (b *broker) deviceView(ex *exitNode) *adminapi.DeviceView {
	active := int(ex.active.Load())
	loadPct := 0.0
	if ex.capacity > 0 {
		loadPct = float64(active) / float64(ex.capacity) * 100
	}
	cpu, mem, disk := ex.sysStat()
	var bps float64
	if st, ok := b.qos.ExitOne(ex.nodeID); ok {
		bps = st.ThroughputBps
	}
	return &adminapi.DeviceView{
		NodeID:        ex.nodeID,
		Name:          ex.name,
		Region:        ex.region,
		Status:        adminapi.NodeOnline,
		System:        ex.osInfo,
		PublicAddr:    ex.conn.RemoteAddr().String(),
		Capacity:      ex.capacity,
		ActiveUsers:   active,
		LoadPct:       loadPct,
		CPUPct:        cpu,
		MemPct:        mem,
		DiskPct:       disk,
		ThroughputBps: bps,
		ConnectedAt:   ex.connectedAt,
		LastSeen:      time.Now().UTC(),
		Config:        adminapi.DeviceConfigSummary{Region: ex.region, Capacity: ex.capacity, VPNType: "quic-datagram", DataplaneMode: "relay"},
	}
}

func (b *broker) sessionView(s *session) *adminapi.Session {
	mode, state := "relay", "active"
	var up, down uint64
	var bps float64
	var rtt int
	var host, tun, osInfo string
	var cpu, mem, disk float64
	if st, ok := b.qos.Session(fmt.Sprintf("%d", s.id)); ok {
		up, down = st.BytesUp, st.BytesDown
		bps, rtt, host, tun, osInfo = st.ThroughputBps, st.RTTms, st.Host, st.TunName, st.OS
		cpu, mem, disk = st.CPUPct, st.MemPct, st.DiskPct
		if st.Path != "" {
			mode = st.Path
		}
		if st.Degraded {
			state = "degraded"
		}
	}
	return &adminapi.Session{
		SessionID:     fmt.Sprintf("%d", s.id),
		UserID:        s.userID,
		Username:      s.username,
		Name:          s.name,
		NodeID:        s.nodeID,
		Region:        s.region,
		Mode:          mode,
		State:         state,
		StartedAt:     s.startedAt,
		BytesUp:       up,
		BytesDown:     down,
		ThroughputBps: bps,
		RTTms:         rtt,
		Host:          host,
		TunName:       tun,
		OS:            osInfo,
		CPUPct:        cpu,
		MemPct:        mem,
		DiskPct:       disk,
	}
}

// --- QoS provider (adminserver.QoSProvider) ---

func (b *broker) QoSExits() []qos.ExitStat         { return b.qos.Exits() }
func (b *broker) QoSSessions() []qos.SessionStat   { return b.qos.Sessions() }
func (b *broker) QoSHistory(limit int) []qos.Event { return b.qos.History(limit) }

func (b *broker) ListNodes() []adminapi.DeviceView {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]adminapi.DeviceView, 0, len(b.exits))
	for _, ex := range b.exits {
		out = append(out, *b.deviceView(ex))
	}
	return out
}

func (b *broker) ListSessions() []adminapi.Session {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]adminapi.Session, 0, len(b.sessions))
	for _, s := range b.sessions {
		out = append(out, *b.sessionView(s))
	}
	return out
}
