// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sourjatilak/revquic/internal/proto"
	"github.com/sourjatilak/revquic/internal/session"
	"github.com/sourjatilak/revquic/internal/telemetry"
)

type noopSender struct{}

func (noopSender) SendDatagram(b []byte) error { return nil }

// newTestExit builds an exit with one relay session that has some traffic + latency, without any
// TUN/broker (so the status layer can be tested on any host).
func newTestExit(t *testing.T) *exit {
	t.Helper()
	e := &exit{
		nodeID: "exit-test-1", displayName: "Test Exit", region: "us-west",
		sessions: map[uint64]*exitSession{}, byIP: map[netip.Addr]uint64{},
	}
	sess := session.New(7)
	if err := sess.StartRelay(session.Path{Sender: noopSender{}, Encode: session.Identity}); err != nil {
		t.Fatalf("StartRelay: %v", err)
	}
	// Generate some downlink volume.
	for i := 0; i < 5; i++ {
		_ = sess.Send(make([]byte, 1000))
	}
	es := &exitSession{
		sess: sess, clientIP: "10.99.0.2", rtt: &telemetry.RTT{}, startedAt: time.Now().Add(-90 * time.Second),
	}
	es.bytesUp.Store(4096)
	es.rtt.Observe(time.Now().Add(-25 * time.Millisecond).UnixNano())
	e.sessions[7] = es
	e.byIP[netip.MustParseAddr("10.99.0.2")] = 7
	return e
}

func TestStatusViews(t *testing.T) {
	e := newTestExit(t)
	info, ss := e.statusViews()
	if info.NodeID != "exit-test-1" || info.Name != "Test Exit" || info.Region != "us-west" {
		t.Fatalf("info identity wrong: %+v", info)
	}
	if info.Clients != 1 || len(ss) != 1 {
		t.Fatalf("clients=%d sessions=%d, want 1/1", info.Clients, len(ss))
	}
	s := ss[0]
	if s.ClientIP != "10.99.0.2" {
		t.Errorf("clientIp=%q", s.ClientIP)
	}
	if s.Mode != "relay" {
		t.Errorf("mode=%q, want relay", s.Mode)
	}
	if s.BytesDown != 5000 {
		t.Errorf("bytesDown=%d, want 5000", s.BytesDown)
	}
	if s.BytesUp != 4096 {
		t.Errorf("bytesUp=%d, want 4096", s.BytesUp)
	}
	if s.LatencyMs <= 0 {
		t.Errorf("latencyMs=%d, want > 0", s.LatencyMs)
	}
	if s.DurationSec < 80 {
		t.Errorf("durationSec=%d, want ~90", s.DurationSec)
	}
}

func TestRemoveSession(t *testing.T) {
	e := newTestExit(t)
	if info, _ := e.statusViews(); info.Clients != 1 {
		t.Fatalf("precondition: clients=%d, want 1", info.Clients)
	}
	e.removeSession(7)
	info, ss := e.statusViews()
	if info.Clients != 0 || len(ss) != 0 {
		t.Fatalf("after removeSession: clients=%d sessions=%d, want 0/0", info.Clients, len(ss))
	}
	// byIP must also be cleared (no stale routing entry).
	e.mu.RLock()
	_, ok := e.byIP[netip.MustParseAddr("10.99.0.2")]
	e.mu.RUnlock()
	if ok {
		t.Errorf("byIP still has the removed client")
	}
	// Removing an unknown session must be a safe no-op.
	e.removeSession(999)
}

func TestResumeIdempotent(t *testing.T) {
	e := newTestExit(t)
	// Mark the existing session parked, then resume it via addSession with the same sid.
	e.sessions[7].suspended = true
	e.addSession(&proto.Control{Type: proto.MsgSessionStart, SessionID: 7, ClientIP: "10.99.0.2"})
	if len(e.sessions) != 1 {
		t.Fatalf("resume must not duplicate: have %d sessions, want 1", len(e.sessions))
	}
	if e.sessions[7].suspended {
		t.Errorf("resume must clear the suspended flag")
	}
}

func TestParkedStatusAccounting(t *testing.T) {
	e := newTestExit(t)
	// Active first.
	if info, _ := e.statusViews(); info.Clients != 1 || info.Parked != 0 {
		t.Fatalf("active: clients=%d parked=%d, want 1/0", info.Clients, info.Parked)
	}
	// Suspend it (client went offline, parked/resumable).
	e.sessions[7].suspended = true
	info, ss := e.statusViews()
	if info.Clients != 0 || info.Parked != 1 {
		t.Fatalf("parked: clients=%d parked=%d, want 0/1", info.Clients, info.Parked)
	}
	if len(ss) != 1 || ss[0].State != "parked" {
		t.Fatalf("session state = %q, want parked", ss[0].State)
	}
}

func TestStatusHandler(t *testing.T) {
	e := newTestExit(t)
	srv := httptest.NewServer(e.statusHandler())
	defer srv.Close()

	// JSON API.
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Info     statusInfo      `json:"info"`
		Sessions []statusSession `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Info.Clients != 1 || len(payload.Sessions) != 1 || payload.Sessions[0].Mode != "relay" {
		t.Fatalf("unexpected payload: %+v", payload)
	}

	// HTML page.
	hr, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer hr.Body.Close()
	body := make([]byte, 4096)
	n, _ := hr.Body.Read(body)
	if !strings.Contains(string(body[:n]), "Revquic Exit") {
		t.Errorf("HTML page missing title marker")
	}
}
