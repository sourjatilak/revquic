// SPDX-License-Identifier: GPL-3.0-or-later

// Command revquic-client is the Revquic client (A).
//
// It dials the broker, requests egress in a region, brings up a TUN, and bootstraps traffic over the
// relay (Phase 0/1). With -direct it additionally runs ICE (controlling) over the broker-relayed
// signaling channel and, on success, migrates the TUN pumps onto a direct QUIC path (Phase 2).
// Linux + root for TUN.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
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
	"github.com/sourjatilak/revquic/internal/socks"
	"github.com/sourjatilak/revquic/internal/sysstat"
	"github.com/sourjatilak/revquic/internal/telemetry"
	"github.com/sourjatilak/revquic/internal/tunnel"
)

func main() {
	brokerAddr := flag.String("broker", "localhost:4242", "broker QUIC address")
	region := flag.String("region", "us-west", "desired egress region")
	full := flag.Bool("full", false, "replace default route with the tunnel (full-tunnel)")
	brokerIP := flag.String("broker-ip", "", "broker public IP for the host route (with -full)")
	gw := flag.String("gw", "", "current default gateway IP (with -full)")
	dev := flag.String("gw-dev", "", "current default route device (with -full)")
	token := flag.String("token", "alice-token", "client user token (validated by the broker)")
	direct := flag.Bool("direct", false, "attempt a direct ICE/QUIC path and migrate off the relay")
	exitID := flag.String("exit", "", "pin to a specific exit nodeId (manual); empty = auto load-balance")
	name := flag.String("name", "", "optional display name for this client (shown in the dashboard)")
	socksAddr := flag.String("socks", "", "run a SOCKS5 proxy at this address (e.g. 127.0.0.1:1080) that routes ONLY apps pointed at it through the tunnel — no default-route change. See USAGE.md.")
	listExits := flag.Bool("list-exits", false, "list available exits in the region, then exit")
	directMode := flag.String("direct-mode", "any", "with -direct: 'any' (use P2P or TURN-relayed) | 'p2p-only' (upgrade only on true peer-to-peer; otherwise stay on the broker relay)")
	iceKeepalive := flag.Duration("ice-keepalive", time.Second, "with -direct: STUN keepalive cadence on the ICE pair (keeps the NAT binding fresh; lower = more resilient on short-timeout NATs)")
	stun := flag.String("stun", "", "comma-separated STUN URLs (e.g. stun:broker:3478)")
	turn := flag.String("turn", "", "comma-separated TURN URLs (e.g. turn:broker:3478)")
	turnUser := flag.String("turn-user", "", "TURN username")
	turnPass := flag.String("turn-pass", "", "TURN password")
	supervise := flag.Bool("supervise", false, "internal: run as the route-cleanup supervisor child (do not set manually)")
	cleanIf := flag.String("clean-if", "", "internal: tun interface name for the cleanup supervisor")
	tlsCA := flag.String("tls-ca", "", "CA cert PEM; with -tls-cert/-tls-key enables mTLS to the broker")
	tlsCert := flag.String("tls-cert", "", "client leaf cert PEM (mTLS)")
	tlsKey := flag.String("tls-key", "", "client leaf key PEM (mTLS)")
	tlsServerName := flag.String("tls-server-name", "broker", "expected broker cert SAN (mTLS)")
	rateBytes := flag.Float64("rate-bytes", 0, "per-session egress cap in bytes/sec (0 = unlimited)")
	reportInterval := flag.Duration("report-interval", 5*time.Second, "QoS report cadence to the broker (0 disables)")
	configPath := flag.String("config", "", "path to a key=value config file (CLI flags take precedence)")
	logFile := flag.String("log-file", "", "write logs to this file (default: stderr)")
	logType := flag.String("log-type", "text", "log format: text | json")
	flag.Parse()
	if *configPath != "" {
		if err := conf.ApplyFile(flag.CommandLine, *configPath); err != nil {
			log.Fatalf("config: %v", err)
		}
	}
	if logClose, err := logx.Setup("client", *logFile, *logType); err != nil {
		log.Fatalf("log: %v", err)
	} else {
		defer logClose()
	}

	// -full (whole-machine, replaces the default route) and -socks (per-app, leaves the default route
	// alone) are mutually exclusive: combining them is contradictory.
	if *full && *socksAddr != "" {
		log.Fatalf("-full and -socks are mutually exclusive: -full sends ALL traffic through the tunnel, " +
			"while -socks routes only the apps you point at the proxy (no default-route change). Pick one.")
	}

	// Supervisor child mode: this process exists only to clean up routes when the primary client
	// exits (for any reason, including crash/SIGKILL). It blocks on the inherited pipe and runs the
	// route teardown on EOF. See spawnSupervisor.
	if *supervise {
		superviseAndCleanup(*cleanIf, *brokerIP, *gw, *dev, *full)
		return
	}

	ctx := context.Background()
	brokerTLS := quicx.ClientTLS()
	directTLS := quicx.ClientTLS()
	if *tlsCA != "" && *tlsCert != "" && *tlsKey != "" {
		mtls, err := quicx.ClientMTLSFromFiles(*tlsCA, *tlsCert, *tlsKey, *tlsServerName)
		if err != nil {
			log.Fatalf("mTLS: %v", err)
		}
		brokerTLS = mtls
		dt, err := quicx.ClientMTLSNoSNIFromFiles(*tlsCA, *tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("direct mTLS: %v", err)
		}
		directTLS = dt
		log.Printf("control-plane mTLS to broker enabled; direct-path mTLS enabled")
	}
	// -list-exits: ask the broker which exits serve the region, print them, and exit. Done before
	// opening the TUN so it needs no root.
	if *listExits {
		listRegionExits(ctx, *brokerAddr, brokerTLS, *region, *token)
		return
	}

	tun, err := tunnel.Open()
	if err != nil {
		log.Fatalf("tun: %v", err)
	}
	defer tun.Close()
	host, _ := os.Hostname()

	// Client host-stats sampler: refreshed every 20s and surfaced to the broker in QoS reports
	// (shows up as the client's CPU/RAM/disk in the dashboard connection detail).
	var statMu sync.Mutex
	var cpuPct, memPct, diskPct float64
	go func() {
		s := &sysstat.Sampler{}
		s.Sample() // prime CPU baseline
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			st := s.Sample()
			statMu.Lock()
			cpuPct, memPct, diskPct = st.CPUPct, st.MemPct, st.DiskPct
			statMu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	hostStat := func() (float64, float64, float64) {
		statMu.Lock()
		defer statMu.Unlock()
		return cpuPct, memPct, diskPct
	}

	// Shared state across reconnects: the current session (read by the TUN pump) and connection
	// (closed by the signal handler / sleep watchdog to force a reconnect).
	var curSess atomic.Pointer[session.Session]
	cb := &connBox{}
	var routesUp bool

	// TUN outbound pump (runs for the whole client lifetime; routes packets to the current session).
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tun.Read(buf)
			if err != nil {
				log.Fatalf("tun read: %v", err)
			}
			s := curSess.Load()
			if s == nil {
				continue // between connections (reconnecting)
			}
			if err := s.Send(buf[:n]); err != nil && err != session.ErrNoPath && err != session.ErrRateLimited {
				var tooLarge *quic.DatagramTooLargeError
				if !errors.As(err, &tooLarge) {
					log.Printf("send: %v", err)
				}
			}
		}
	}()

	// Optional per-application proxy (once): a local SOCKS5 server whose outbound sockets are bound
	// to the tunnel interface, so ONLY apps pointed at it egress through the exit. No default-route
	// change — works alongside split-tunnel without -full.
	if *socksAddr != "" {
		srv, err := socks.New(*socksAddr, tun.Name(), log.Printf)
		if err != nil {
			log.Fatalf("socks: %v", err)
		}
		log.Printf("SOCKS5 proxy listening on %s → tunnel %s (point an app at socks5://%s)", srv.Addr(), tun.Name(), srv.Addr())
		go func() {
			if err := srv.Serve(); err != nil {
				log.Printf("socks: server stopped: %v", err)
			}
		}()
	}

	// Graceful shutdown UX (once): SIGTERM exits immediately; Ctrl-C/Ctrl-Z need a confirming second press.
	var shuttingDown atomic.Bool
	shutdown.OnSignals(3*time.Second, log.Printf, func() {
		log.Printf("shutting down: closing broker connection")
		shuttingDown.Store(true)
		cb.closeCode(proto.CloseClientShutdown, "client shutting down")
		os.Exit(0)
	})

	// Sleep/wake watchdog (once): a large wall-clock jump means the host slept; drop the current
	// connection so the reconnect loop establishes a fresh session on wake.
	go func() {
		last := time.Now()
		for {
			time.Sleep(2 * time.Second)
			now := time.Now()
			if now.Sub(last) > 10*time.Second {
				log.Printf("wake detected (paused ~%s) — reconnecting", now.Sub(last).Round(time.Second))
				cb.close("wake-reconnect")
			}
			last = now
		}
	}()

	// Reconnect loop: (re)establish the broker connection + session and run until it drops, then
	// retry with capped backoff. On the FIRST attempt a "no exit" error is fatal (the user asked to
	// connect and none is available); after we've been connected, drops trigger a reconnect instead.
	p := runParams{
		brokerAddr: *brokerAddr, brokerTLS: brokerTLS, directTLS: directTLS,
		region: *region, token: *token, exitID: *exitID, name: *name, full: *full,
		resumeKey: newResumeKey(),
		brokerIP:  *brokerIP, gw: *gw, dev: *dev, logFile: *logFile, logType: *logType,
		direct: *direct, directMode: *directMode, stun: *stun, turn: *turn, turnUser: *turnUser, turnPass: *turnPass,
		iceKeepalive: *iceKeepalive,
		rateBytes:    *rateBytes, reportInterval: *reportInterval,
		tun: tun, host: host, hostStat: hostStat, curSess: &curSess, cb: cb, routesUp: &routesUp,
	}
	backoff := time.Second
	first := true
	for {
		err := runConnection(ctx, p)
		curSess.Store(nil)
		if shuttingDown.Load() {
			return // intentional Ctrl-C/SIGTERM exit; don't log a misleading reconnect
		}
		if ne, ok := err.(noExitErr); ok && first {
			log.Fatalf("cannot connect: %s", ne.msg)
		}
		first = false
		log.Printf("connection down: %s — reconnecting in %s", quicx.Explain(err, p.brokerAddr), backoff)
		time.Sleep(backoff)
		if backoff < 15*time.Second {
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		}
	}
}

// connBox holds the current broker connection so the signal handler and sleep watchdog can close it
// (which makes the active runConnection return and the reconnect loop re-establish a session).
type connBox struct {
	mu sync.Mutex
	c  quic.Connection
}

func (b *connBox) set(c quic.Connection) { b.mu.Lock(); b.c = c; b.mu.Unlock() }
func (b *connBox) close(reason string)   { b.closeCode(0, reason) }

// closeCode closes the current connection with a specific QUIC application error code so the broker
// can tell an intentional client shutdown apart from a transient drop / sleep-wake reconnect.
func (b *connBox) closeCode(code uint64, reason string) {
	b.mu.Lock()
	if b.c != nil {
		_ = b.c.CloseWithError(quic.ApplicationErrorCode(code), reason)
	}
	b.mu.Unlock()
}

// noExitErr signals the broker reported no usable exit (vs a transport error).
type noExitErr struct{ msg string }

func (e noExitErr) Error() string { return e.msg }

type runParams struct {
	brokerAddr                     string
	brokerTLS, directTLS           *tls.Config
	region, token, exitID, name    string
	resumeKey                      string
	full                           bool
	brokerIP, gw, dev              string
	logFile, logType               string
	direct                         bool
	directMode                     string
	stun, turn, turnUser, turnPass string
	iceKeepalive                   time.Duration
	rateBytes                      float64
	reportInterval                 time.Duration
	tun                            *tunnel.Device
	host                           string
	hostStat                       func() (float64, float64, float64)
	curSess                        *atomic.Pointer[session.Session]
	cb                             *connBox
	routesUp                       *bool
}

// runConnection establishes one broker connection + session and runs it until the connection drops
// (returning the cause). The TUN device and full-tunnel routes persist across calls.
func runConnection(ctx context.Context, p runParams) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, err := quic.DialAddr(cctx, p.brokerAddr, p.brokerTLS, quicx.Config())
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "")
	ctrl, err := conn.OpenStreamSync(cctx)
	if err != nil {
		return err
	}
	if err := proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgConnect, RequestedRegion: p.region, Token: p.token, RequestedExit: p.exitID, Name: p.name, ResumeKey: p.resumeKey}); err != nil {
		return err
	}
	resp, err := proto.ReadControl(ctrl)
	if err != nil {
		return err
	}
	if resp.Type != proto.MsgConnectOK {
		return noExitErr{resp.Error}
	}
	sid := resp.SessionID
	cidr := resp.ClientIP + "/24"
	mtu := resp.MTU
	if mtu == 0 || mtu > proto.SafeTunnelMTU {
		mtu = proto.SafeTunnelMTU
	}
	if err := netcfg.AddrUp(p.tun.Name(), cidr, mtu); err != nil {
		log.Printf("addr: %v (continuing)", err) // tolerate "already exists" on reconnect
	}
	if p.full && !*p.routesUp {
		if p.brokerIP != "" && p.gw != "" && p.dev != "" {
			if err := netcfg.AddHostRoute(p.brokerIP, p.gw, p.dev); err != nil {
				log.Printf("host route: %v (continuing)", err)
			}
		} else {
			log.Printf("WARNING: -full without -broker-ip/-gw/-gw-dev may cut your connectivity")
		}
		if err := netcfg.SetDefaultRoute(p.tun.Name()); err != nil {
			log.Fatalf("default route: %v", err)
		}
		log.Printf("full-tunnel: default route via %s", p.tun.Name())
		if pw, err := spawnSupervisor(p.tun.Name(), p.brokerIP, p.gw, p.dev, p.full, p.logFile, p.logType); err != nil {
			log.Printf("WARNING: cleanup supervisor not started: %v", err)
		} else {
			supervisorPipe = pw
			log.Printf("route-cleanup supervisor started (pid-guarded)")
		}
		*p.routesUp = true
	}
	log.Printf("session %d: assigned %s (region %s)", sid, cidr, p.region)

	relayPath := session.Path{Sender: conn, Encode: relayEncode(sid)}
	sess := session.New(sid)
	if err := sess.StartRelay(relayPath); err != nil {
		return err
	}
	if p.rateBytes > 0 {
		sess.SetRateLimit(p.rateBytes, p.rateBytes*2)
	}
	p.curSess.Store(sess)
	p.cb.set(conn)

	cw := &ctrlWriter{s: ctrl}
	rtt := &telemetry.RTT{}
	meta := telemetry.Meta{
		Host: p.host, TunName: p.tun.Name(), OS: runtime.GOOS + "/" + runtime.GOARCH,
		UsingStun: p.direct && orStr(resp.StunURL, p.stun) != "",
		UsingTurn: p.direct && orStr(resp.TurnURL, p.turn) != "",
		HostStat:  p.hostStat,
	}
	if p.reportInterval > 0 {
		go telemetry.Run(cctx, cw, sid, p.reportInterval, sess, meta, rtt)
	}

	incoming := make(chan *ice.Signal, 64)
	if p.direct {
		stunEff, turnEff := orStr(resp.StunURL, p.stun), orStr(resp.TurnURL, p.turn)
		userEff, passEff := orStr(resp.TurnUser, p.turnUser), orStr(resp.TurnPass, p.turnPass)
		go runDirect(cctx, sess, p.tun, cw, sid, stunEff, turnEff, userEff, passEff, p.directTLS, incoming, p.directMode == "p2p-only", relayPath, p.iceKeepalive)
	}

	// relay inbound: broker datagrams (sid-prefixed) -> TUN.
	go func() {
		for {
			dg, err := conn.ReceiveDatagram(cctx)
			if err != nil {
				return
			}
			if _, pkt, err := proto.DecodeDatagram(dg); err == nil {
				_, _ = p.tun.Write(pkt)
			}
		}
	}()

	// Control reader: blocks until the connection drops; its return drives the reconnect loop.
	for {
		m, err := proto.ReadControl(ctrl)
		if err != nil {
			return err
		}
		switch m.Type {
		case proto.MsgSignal:
			var s ice.Signal
			if json.Unmarshal(m.Signal, &s) == nil {
				select {
				case incoming <- &s:
				default:
				}
			}
		case proto.MsgError:
			return noExitErr{m.Error}
		case proto.MsgPong:
			rtt.Observe(m.TS)
		}
	}
}

// listRegionExits prints the exits the broker offers in the region, then returns (for -list-exits).
func listRegionExits(ctx context.Context, brokerAddr string, tlsConf *tls.Config, region, token string) {
	conn, err := quic.DialAddr(ctx, brokerAddr, tlsConf, quicx.Config())
	if err != nil {
		log.Fatalf("dial broker: %v", err)
	}
	defer conn.CloseWithError(0, "")
	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		log.Fatalf("open control: %v", err)
	}
	if err := proto.WriteControl(ctrl, &proto.Control{Type: proto.MsgConnect, RequestedRegion: region, Token: token, ListExits: true}); err != nil {
		log.Fatalf("list-exits: %v", err)
	}
	resp, err := proto.ReadControl(ctrl)
	if err != nil {
		log.Fatalf("list-exits resp: %v", err)
	}
	if resp.Type != proto.MsgConnectOK {
		log.Fatalf("broker: %s", resp.Error)
	}
	if len(resp.ExitList) == 0 {
		log.Printf("no exits available in region %q", region)
		return
	}
	log.Printf("exits in region %q:", region)
	for _, e := range resp.ExitList {
		label := e.NodeID
		if e.Name != "" {
			label = fmt.Sprintf("%s (%s)", e.Name, e.NodeID)
		}
		log.Printf("  %-30s  users=%d/%d  system=%s", label, e.ActiveUsers, e.Capacity, e.System)
	}
	log.Printf("select one with: -exit <nodeId>   (omit -exit for automatic load-balancing)")
}

func relayEncode(sid uint64) func([]byte) []byte {
	return func(pkt []byte) []byte { return proto.EncodeDatagram(sid, pkt) }
}

// newResumeKey returns a random per-process session resume key. The broker uses it to reattach this
// client to the same session (same exit + VPN IP) if it reconnects within the resume window.
func newResumeKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "" // empty key -> broker treats every connect as new (no resume)
	}
	return hex.EncodeToString(b[:])
}

// supervisorPipe is the write end of the pipe shared with the cleanup supervisor child. It is held
// open for the parent's entire lifetime; when the parent exits (cleanly, crash, or SIGKILL) the OS
// closes it, signaling the supervisor (which sees EOF) to run route cleanup.
var supervisorPipe *os.File

// spawnSupervisor launches a second copy of this binary in -supervise mode, handing it the read end
// of a pipe as fd 3. Returns the write end (keep it open). Unix-only; on Windows the Wintun adapter
// and its interface-scoped routes are removed automatically on process exit, so no supervisor is
// needed and this returns a nil pipe without error.
func spawnSupervisor(ifName, brokerIP, gw, dev string, full bool, logFile, logType string) (*os.File, error) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(os.Args[0],
		"-supervise",
		"-clean-if", ifName,
		"-broker-ip", brokerIP,
		"-gw", gw,
		"-gw-dev", dev,
		fmt.Sprintf("-full=%t", full),
		"-log-file", logFile,
		"-log-type", logType,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.ExtraFiles = []*os.File{pr} // child sees this as fd 3
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	pr.Close() // parent keeps only the write end
	return pw, nil
}

// superviseAndCleanup runs in the supervisor child. It blocks reading the inherited pipe (fd 3)
// until the parent exits (EOF), then performs best-effort route teardown so a dead/disconnected
// client never leaves the host with broken routing.
func superviseAndCleanup(ifName, brokerIP, gw, dev string, full bool) {
	if f := os.NewFile(3, "parent-pipe"); f != nil {
		buf := make([]byte, 1)
		for {
			if _, err := f.Read(buf); err != nil {
				break // EOF or error => parent gone
			}
		}
	}
	log.Printf("cleanup supervisor: primary client exited — restoring routes (if=%s full=%t)", ifName, full)
	netcfg.Cleanup(ifName, brokerIP, gw, dev, full)
}

// ctrlWriter serializes writes to the broker control stream (ICE signaling + QoS reports).
type ctrlWriter struct {
	mu sync.Mutex
	s  quic.Stream
}

func (c *ctrlWriter) WriteControl(m *proto.Control) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return proto.WriteControl(c.s, m)
}

// runDirect negotiates the ICE/QUIC direct path (controlling) and migrates the session onto it.
func runDirect(ctx context.Context, sess *session.Session, tun *tunnel.Device, cw *ctrlWriter, sid uint64, stun, turn, turnUser, turnPass string, directTLS *tls.Config, incoming <-chan *ice.Signal, p2pOnly bool, relayPath session.Path, iceKA time.Duration) {
	agent, err := ice.NewPionAgent(ice.PionConfig{
		Role: ice.RoleControlling, STUNURLs: splitCSV(stun), TURNURLs: splitCSV(turn),
		TURNUser: turnUser, TURNPass: turnPass, KeepaliveInterval: iceKA,
	})
	if err != nil {
		log.Printf("direct: agent: %v", err)
		return
	}
	defer agent.Close()

	send := func(s *ice.Signal) error {
		b, _ := json.Marshal(s)
		return cw.WriteControl(&proto.Control{Type: proto.MsgSignal, SessionID: sid, Signal: b})
	}

	if err := sess.BeginChecks(); err != nil {
		log.Printf("direct: begin checks: %v", err)
		return
	}
	nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	qc, err := icewire.Negotiate(nctx, agent, ice.RoleControlling, "1", send, incoming, directTLS, p2pOnly)
	if err != nil {
		if errors.Is(err, directlink.ErrRelayPair) {
			log.Printf("direct: only a TURN-relayed path is available — staying on the broker relay (-direct-mode p2p-only)")
		} else {
			log.Printf("direct: negotiate failed, staying on relay: %v", err)
		}
		_ = sess.ChecksFailed()
		return
	}
	if err := sess.UpgradeDirect(session.Path{Sender: qc, Encode: session.Identity}); err != nil {
		log.Printf("direct: upgrade: %v", err)
		return
	}
	log.Printf("session %d: upgraded to DIRECT path", sid)
	// direct inbound: raw IP datagrams -> TUN
	for {
		dg, err := qc.ReceiveDatagram(ctx)
		if err != nil {
			// Direct path died — re-point the session back to the relay so the TUN pump keeps
			// working instead of black-holing every packet on the dead direct connection.
			if ferr := sess.FallbackRelay(relayPath); ferr != nil {
				log.Printf("direct recv: %v; relay fallback failed: %v", err, ferr)
			} else {
				log.Printf("direct recv: %v; fell back to relay", err)
			}
			return
		}
		_, _ = tun.Write(dg)
	}
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
