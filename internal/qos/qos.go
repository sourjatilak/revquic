// SPDX-License-Identifier: GPL-3.0-or-later

// Package qos is the broker's quality-of-service tracker. It records per-exit load, per-session
// live statistics (throughput, byte totals, drops, RTT), detects throughput "speed drops", and
// keeps a capped, time-ordered history of lifecycle events (node up/down, client connect/
// disconnect, speed drop/recovered). It is a dependency-free leaf package so the broker, the
// relay loop, and the admin server can all share one tracker.
//
// Two complementary inputs feed it:
//   - AddBytes: called by the broker relay loop for every datagram it forwards. This gives
//     authoritative byte totals and throughput for relayed sessions with no endpoint cooperation.
//   - Report: called when a client/exit sends a periodic MsgReport. This covers the DIRECT path
//     (which bypasses the broker) and adds drops/RTT the broker cannot observe itself.
package qos

import (
	"sort"
	"sync"
	"time"
)

// Event kinds.
const (
	KindNodeUp     = "node_up"
	KindNodeDown   = "node_down"
	KindConnect    = "connect"
	KindDisconnect = "disconnect"
	KindSpeedDrop  = "speed_drop"
	KindRecovered  = "recovered"
)

// Path values for a session's current data path.
const (
	PathRelay  = "relay"
	PathDirect = "direct"
)

// Defaults for the tracker.
const (
	DefaultHistory    = 1000
	defaultDropFactor = 0.5   // degraded if throughput < 50% of the session's peak
	defaultRecoverPct = 0.8   // recovered once back above 80% of peak
	defaultFloorBps   = 65536 // ignore speed drops below 64 KB/s (noise on idle links)
	windowDur         = time.Second
)

// SessionStat is the public, JSON-serialized live view of one session.
type SessionStat struct {
	SessionID     string    `json:"sessionId"`
	NodeID        string    `json:"nodeId"`
	Region        string    `json:"region"`
	Username      string    `json:"username,omitempty"`
	Path          string    `json:"path"`
	StartedAt     time.Time `json:"startedAt"`
	DurationSec   int64     `json:"durationSec"`
	BytesUp       uint64    `json:"bytesUp"`
	BytesDown     uint64    `json:"bytesDown"`
	ThroughputBps float64   `json:"throughputBps"`
	PeakBps       float64   `json:"peakBps"`
	Drops         uint64    `json:"drops"`
	RTTms         int       `json:"rttMs,omitempty"`
	Host          string    `json:"host,omitempty"`
	TunName       string    `json:"tunName,omitempty"`
	OS            string    `json:"os,omitempty"`
	CPUPct        float64   `json:"cpuPct,omitempty"`
	MemPct        float64   `json:"memPct,omitempty"`
	DiskPct       float64   `json:"diskPct,omitempty"`
	Degraded      bool      `json:"degraded"`
	LastSeen      time.Time `json:"lastSeen"`
}

// ExitStat is the public, JSON-serialized load/throughput view of one exit node.
type ExitStat struct {
	NodeID        string    `json:"nodeId"`
	Region        string    `json:"region"`
	ActiveClients int       `json:"activeClients"`
	BytesUp       uint64    `json:"bytesUp"`
	BytesDown     uint64    `json:"bytesDown"`
	ThroughputBps float64   `json:"throughputBps"`
	PeakBps       float64   `json:"peakBps"`
	ConnectedAt   time.Time `json:"connectedAt"`
	LastSeen      time.Time `json:"lastSeen"`
}

// Event is one history entry.
type Event struct {
	TS            time.Time `json:"ts"`
	Kind          string    `json:"kind"`
	SessionID     string    `json:"sessionId,omitempty"`
	NodeID        string    `json:"nodeId,omitempty"`
	Region        string    `json:"region,omitempty"`
	Username      string    `json:"username,omitempty"`
	ThroughputBps float64   `json:"throughputBps,omitempty"`
	Detail        string    `json:"detail,omitempty"`
}

// Report is an endpoint-supplied telemetry sample (from a client or exit MsgReport).
type Report struct {
	BytesUp       uint64
	BytesDown     uint64
	ThroughputBps float64
	Drops         uint64
	RTTms         int
	Direct        bool
	Host          string
	TunName       string
	OS            string
	CPUPct        float64
	MemPct        float64
	DiskPct       float64
}

type sessionState struct {
	id        string
	nodeID    string
	region    string
	username  string
	path      string
	startedAt time.Time
	lastSeen  time.Time

	bytesUp   uint64
	bytesDown uint64
	bps       float64
	peakBps   float64
	drops     uint64
	rttMs     int
	host      string
	tunName   string
	os        string
	cpu       float64
	mem       float64
	disk      float64
	degraded  bool

	// relay throughput windowing (AddBytes)
	winStart time.Time
	winBytes uint64
}

type exitState struct {
	nodeID      string
	region      string
	active      int
	bytesUp     uint64
	bytesDown   uint64
	bps         float64
	peakBps     float64
	connectedAt time.Time
	lastSeen    time.Time
	winStart    time.Time
	winBytes    uint64
}

// Tracker is the concurrency-safe QoS store.
type Tracker struct {
	mu  sync.Mutex
	now func() time.Time

	histCap    int
	hist       []Event
	dropFactor float64
	recoverPct float64
	floorBps   float64

	sessions map[string]*sessionState
	exits    map[string]*exitState

	store HistoryStore // optional: persists events across restarts
}

// New returns a tracker keeping up to histCap history events (<=0 uses DefaultHistory).
func New(histCap int) *Tracker {
	if histCap <= 0 {
		histCap = DefaultHistory
	}
	return &Tracker{
		now:        func() time.Time { return time.Now().UTC() },
		histCap:    histCap,
		dropFactor: defaultDropFactor,
		recoverPct: defaultRecoverPct,
		floorBps:   defaultFloorBps,
		sessions:   map[string]*sessionState{},
		exits:      map[string]*exitState{},
	}
}

// WithClock overrides the time source (tests). Returns the tracker for chaining.
func (t *Tracker) WithClock(now func() time.Time) *Tracker {
	t.mu.Lock()
	t.now = now
	t.mu.Unlock()
	return t
}

// NodeUp records an exit node coming online.
func (t *Tracker) NodeUp(nodeID, region string, connectedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := t.now()
	t.exits[nodeID] = &exitState{nodeID: nodeID, region: region, connectedAt: connectedAt, lastSeen: ts, winStart: ts}
	t.appendEvent(Event{TS: ts, Kind: KindNodeUp, NodeID: nodeID, Region: region})
}

// NodeDown records an exit node going offline.
func (t *Tracker) NodeDown(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	region := ""
	if e := t.exits[nodeID]; e != nil {
		region = e.region
	}
	delete(t.exits, nodeID)
	t.appendEvent(Event{TS: t.now(), Kind: KindNodeDown, NodeID: nodeID, Region: region})
}

// Connect registers a new client session and bumps the exit's active count.
func (t *Tracker) Connect(sessionID, nodeID, region, username string, startedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := t.now()
	t.sessions[sessionID] = &sessionState{
		id: sessionID, nodeID: nodeID, region: region, username: username,
		path: PathRelay, startedAt: startedAt, lastSeen: ts, winStart: ts,
	}
	if e := t.exits[nodeID]; e != nil {
		e.active++
		e.lastSeen = ts
	}
	t.appendEvent(Event{TS: ts, Kind: KindConnect, SessionID: sessionID, NodeID: nodeID, Region: region, Username: username})
}

// Disconnect finalizes a session and decrements its exit's active count.
func (t *Tracker) Disconnect(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ss := t.sessions[sessionID]
	if ss == nil {
		return
	}
	delete(t.sessions, sessionID)
	if e := t.exits[ss.nodeID]; e != nil && e.active > 0 {
		e.active--
	}
	t.appendEvent(Event{TS: t.now(), Kind: KindDisconnect, SessionID: sessionID, NodeID: ss.nodeID, Region: ss.region, Username: ss.username})
}

// AddBytes accounts a relayed datagram of n bytes. fromClient=true counts toward "up" (client->
// exit), false toward "down". Throughput is computed over ~1s windows and feeds speed-drop
// detection for both the session and its exit.
func (t *Tracker) AddBytes(sessionID string, fromClient bool, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts := t.now()
	ss := t.sessions[sessionID]
	if ss == nil {
		return
	}
	ss.lastSeen = ts
	if fromClient {
		ss.bytesUp += uint64(n)
	} else {
		ss.bytesDown += uint64(n)
	}
	ss.winBytes += uint64(n)
	if d := ts.Sub(ss.winStart); d >= windowDur {
		bps := float64(ss.winBytes) / d.Seconds()
		t.sampleSession(ss, bps, ts)
		ss.winStart = ts
		ss.winBytes = 0
	}

	if e := t.exits[ss.nodeID]; e != nil {
		e.lastSeen = ts
		if fromClient {
			e.bytesUp += uint64(n)
		} else {
			e.bytesDown += uint64(n)
		}
		e.winBytes += uint64(n)
		if d := ts.Sub(e.winStart); d >= windowDur {
			e.bps = float64(e.winBytes) / d.Seconds()
			if e.bps > e.peakBps {
				e.peakBps = e.bps
			}
			e.winStart = ts
			e.winBytes = 0
		}
	}
}

// Report ingests an endpoint telemetry sample. Byte totals only grow (max of relay-observed and
// reported). A non-zero ThroughputBps feeds speed-drop detection; Direct marks the path.
func (t *Tracker) Report(sessionID string, r Report) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ss := t.sessions[sessionID]
	if ss == nil {
		return
	}
	ts := t.now()
	ss.lastSeen = ts
	if r.BytesUp > ss.bytesUp {
		ss.bytesUp = r.BytesUp
	}
	if r.BytesDown > ss.bytesDown {
		ss.bytesDown = r.BytesDown
	}
	if r.Drops > ss.drops {
		ss.drops = r.Drops
	}
	if r.RTTms > 0 {
		ss.rttMs = r.RTTms
	}
	if r.Host != "" {
		ss.host = r.Host
	}
	if r.TunName != "" {
		ss.tunName = r.TunName
	}
	if r.OS != "" {
		ss.os = r.OS
	}
	if r.CPUPct > 0 {
		ss.cpu = r.CPUPct
	}
	if r.MemPct > 0 {
		ss.mem = r.MemPct
	}
	if r.DiskPct > 0 {
		ss.disk = r.DiskPct
	}
	if r.Direct {
		ss.path = PathDirect
	}
	if r.ThroughputBps > 0 {
		t.sampleSession(ss, r.ThroughputBps, ts)
	}
}

// sampleSession updates a session's throughput/peak and emits speed-drop / recovered events.
// Caller holds the lock.
func (t *Tracker) sampleSession(ss *sessionState, bps float64, ts time.Time) {
	ss.bps = bps
	if bps > ss.peakBps {
		ss.peakBps = bps
	}
	if ss.peakBps < t.floorBps {
		return
	}
	switch {
	case !ss.degraded && bps < ss.peakBps*t.dropFactor:
		ss.degraded = true
		t.appendEvent(Event{TS: ts, Kind: KindSpeedDrop, SessionID: ss.id, NodeID: ss.nodeID, Region: ss.region, Username: ss.username, ThroughputBps: bps})
	case ss.degraded && bps >= ss.peakBps*t.recoverPct:
		ss.degraded = false
		t.appendEvent(Event{TS: ts, Kind: KindRecovered, SessionID: ss.id, NodeID: ss.nodeID, Region: ss.region, Username: ss.username, ThroughputBps: bps})
	}
}

// appendEvent pushes to the capped ring and (if configured) to the persistent store. The store's
// Append is non-blocking, so it is safe to call while the lock is held. Caller holds the lock.
func (t *Tracker) appendEvent(ev Event) {
	t.hist = append(t.hist, ev)
	if len(t.hist) > t.histCap {
		t.hist = t.hist[len(t.hist)-t.histCap:]
	}
	if t.store != nil {
		t.store.Append(ev)
	}
}

// CloseStore flushes and closes the persistent history store, if one is attached. Safe to call
// once at shutdown; subsequent event Appends become no-ops.
func (t *Tracker) CloseStore() error {
	t.mu.Lock()
	s := t.store
	t.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Close()
}

// WithStore attaches a persistent history store. Returns the tracker for chaining.
func (t *Tracker) WithStore(s HistoryStore) *Tracker {
	t.mu.Lock()
	t.store = s
	t.mu.Unlock()
	return t
}

// Preload seeds the in-memory ring from the persistent store (most-recent limit events) so a
// restarted broker keeps its recent history. Call once at startup before traffic flows.
func (t *Tracker) Preload(limit int) error {
	if t.store == nil {
		return nil
	}
	evs, err := t.store.Load(limit) // newest-first
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Rebuild the ring oldest-first (History reverses to newest-first on read).
	t.hist = t.hist[:0]
	for i := len(evs) - 1; i >= 0; i-- {
		t.hist = append(t.hist, evs[i])
	}
	if len(t.hist) > t.histCap {
		t.hist = t.hist[len(t.hist)-t.histCap:]
	}
	return nil
}

// Sessions returns the live session stats, sorted by session id.
func (t *Tracker) Sessions() []SessionStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	out := make([]SessionStat, 0, len(t.sessions))
	for _, ss := range t.sessions {
		out = append(out, SessionStat{
			SessionID: ss.id, NodeID: ss.nodeID, Region: ss.region, Username: ss.username, Path: ss.path,
			StartedAt: ss.startedAt, DurationSec: int64(now.Sub(ss.startedAt).Seconds()),
			BytesUp: ss.bytesUp, BytesDown: ss.bytesDown, ThroughputBps: ss.bps, PeakBps: ss.peakBps,
			Drops: ss.drops, RTTms: ss.rttMs, Degraded: ss.degraded, LastSeen: ss.lastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// Exits returns the per-exit load/throughput stats, sorted by node id.
// ExitOne returns the live stats for a single exit by id.
func (t *Tracker) ExitOne(id string) (ExitStat, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.exits[id]
	if e == nil {
		return ExitStat{}, false
	}
	return ExitStat{
		NodeID: e.nodeID, Region: e.region, ActiveClients: e.active,
		BytesUp: e.bytesUp, BytesDown: e.bytesDown, ThroughputBps: e.bps, PeakBps: e.peakBps,
		ConnectedAt: e.connectedAt, LastSeen: e.lastSeen,
	}, true
}

func (t *Tracker) Exits() []ExitStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ExitStat, 0, len(t.exits))
	for _, e := range t.exits {
		out = append(out, ExitStat{
			NodeID: e.nodeID, Region: e.region, ActiveClients: e.active,
			BytesUp: e.bytesUp, BytesDown: e.bytesDown, ThroughputBps: e.bps, PeakBps: e.peakBps,
			ConnectedAt: e.connectedAt, LastSeen: e.lastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Session returns the live stats for one session id.
func (t *Tracker) Session(id string) (SessionStat, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ss := t.sessions[id]
	if ss == nil {
		return SessionStat{}, false
	}
	now := t.now()
	return SessionStat{
		SessionID: ss.id, NodeID: ss.nodeID, Region: ss.region, Username: ss.username, Path: ss.path,
		StartedAt: ss.startedAt, DurationSec: int64(now.Sub(ss.startedAt).Seconds()),
		BytesUp: ss.bytesUp, BytesDown: ss.bytesDown, ThroughputBps: ss.bps, PeakBps: ss.peakBps,
		Drops: ss.drops, RTTms: ss.rttMs, Host: ss.host, TunName: ss.tunName, OS: ss.os, CPUPct: ss.cpu, MemPct: ss.mem, DiskPct: ss.disk, Degraded: ss.degraded, LastSeen: ss.lastSeen,
	}, true
}

// ActiveClients returns the current active-session count for an exit (0 if unknown).
func (t *Tracker) ActiveClients(nodeID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.exits[nodeID]; e != nil {
		return e.active
	}
	return 0
}

// History returns up to limit most-recent events, newest first (<=0 returns all).
func (t *Tracker) History(limit int) []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.hist)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, t.hist[len(t.hist)-1-i]) // newest first
	}
	return out
}
