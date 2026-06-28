// SPDX-License-Identifier: GPL-3.0-or-later

package qos

import (
	"testing"
	"time"
)

// fakeClock returns a controllable time source.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) add(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}

func newTestTracker() (*Tracker, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := New(100).WithClock(c.now)
	return tr, c
}

func TestConnectDisconnectUpdatesExitLoad(t *testing.T) {
	tr, _ := newTestTracker()
	tr.NodeUp("exit-1", "us-west", time.Now())
	tr.Connect("1", "exit-1", "us-west", "alice", time.Now())
	tr.Connect("2", "exit-1", "us-west", "bob", time.Now())
	if got := tr.ActiveClients("exit-1"); got != 2 {
		t.Fatalf("active = %d, want 2", got)
	}
	tr.Disconnect("1")
	if got := tr.ActiveClients("exit-1"); got != 1 {
		t.Fatalf("active after disconnect = %d, want 1", got)
	}
	// Exits() should reflect the live count.
	ex := tr.Exits()
	if len(ex) != 1 || ex[0].ActiveClients != 1 {
		t.Fatalf("Exits() = %+v", ex)
	}
}

func TestAddBytesAccumulatesTotalsAndDirection(t *testing.T) {
	tr, _ := newTestTracker()
	tr.NodeUp("exit-1", "us-west", time.Now())
	tr.Connect("1", "exit-1", "us-west", "alice", time.Now())
	tr.AddBytes("1", true, 1000)  // up
	tr.AddBytes("1", false, 4000) // down
	s := findSession(t, tr, "1")
	if s.BytesUp != 1000 || s.BytesDown != 4000 {
		t.Fatalf("bytes up/down = %d/%d, want 1000/4000", s.BytesUp, s.BytesDown)
	}
	ex := tr.Exits()[0]
	if ex.BytesUp != 1000 || ex.BytesDown != 4000 {
		t.Fatalf("exit bytes = %d/%d", ex.BytesUp, ex.BytesDown)
	}
}

func TestReportMergesDropsRTTAndDirectPath(t *testing.T) {
	tr, _ := newTestTracker()
	tr.NodeUp("exit-1", "us-west", time.Now())
	tr.Connect("1", "exit-1", "us-west", "alice", time.Now())
	tr.Report("1", Report{Drops: 5, RTTms: 42, Direct: true, BytesDown: 9999})
	s := findSession(t, tr, "1")
	if s.Drops != 5 || s.RTTms != 42 || s.Path != PathDirect || s.BytesDown != 9999 {
		t.Fatalf("report not merged: %+v", s)
	}
}

func TestSpeedDropAndRecoveryEvents(t *testing.T) {
	tr, _ := newTestTracker()
	tr.NodeUp("exit-1", "us-west", time.Now())
	tr.Connect("1", "exit-1", "us-west", "alice", time.Now())

	// Establish a high peak well above the floor (1 MB/s), then drop to 10% -> speed_drop.
	tr.Report("1", Report{ThroughputBps: 1_000_000})
	tr.Report("1", Report{ThroughputBps: 100_000}) // 10% of peak -> degraded
	if !findSession(t, tr, "1").Degraded {
		t.Fatalf("session should be degraded after throughput drop")
	}
	if countKind(tr, KindSpeedDrop) != 1 {
		t.Fatalf("want exactly 1 speed_drop event, got %d", countKind(tr, KindSpeedDrop))
	}
	// A second low sample must NOT emit another speed_drop (debounced).
	tr.Report("1", Report{ThroughputBps: 120_000})
	if countKind(tr, KindSpeedDrop) != 1 {
		t.Fatalf("speed_drop should be debounced; got %d", countKind(tr, KindSpeedDrop))
	}
	// Recover above 80% of peak -> recovered event, cleared flag.
	tr.Report("1", Report{ThroughputBps: 900_000})
	if findSession(t, tr, "1").Degraded {
		t.Fatalf("session should have recovered")
	}
	if countKind(tr, KindRecovered) != 1 {
		t.Fatalf("want 1 recovered event, got %d", countKind(tr, KindRecovered))
	}
}

func TestSpeedDropIgnoredBelowFloor(t *testing.T) {
	tr, _ := newTestTracker()
	tr.NodeUp("exit-1", "us-west", time.Now())
	tr.Connect("1", "exit-1", "us-west", "alice", time.Now())
	// Peak below the 64 KB/s floor: drops are treated as idle noise, no event.
	tr.Report("1", Report{ThroughputBps: 10_000})
	tr.Report("1", Report{ThroughputBps: 100})
	if countKind(tr, KindSpeedDrop) != 0 {
		t.Fatalf("no speed_drop expected below floor, got %d", countKind(tr, KindSpeedDrop))
	}
}

func TestAddBytesWindowedThroughputDetectsDrop(t *testing.T) {
	tr, c := newTestTracker()
	tr.NodeUp("exit-1", "us-west", time.Now())
	tr.Connect("1", "exit-1", "us-west", "alice", c.now())
	// Window 1: 1 MB in 1s -> 1 MB/s peak.
	tr.AddBytes("1", true, 1_000_000)
	c.add(time.Second)
	tr.AddBytes("1", true, 1) // crosses the window boundary, samples ~1MB/s
	// Window 2: only 1 KB in 1s -> ~1 KB/s, far below 50% of peak -> speed_drop.
	c.add(time.Second)
	tr.AddBytes("1", true, 1000)
	if countKind(tr, KindSpeedDrop) < 1 {
		t.Fatalf("expected a speed_drop from windowed throughput, got %d", countKind(tr, KindSpeedDrop))
	}
}

func TestHistoryOrderingAndCap(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tr := New(3).WithClock(c.now) // tiny cap
	tr.NodeUp("a", "r", time.Now())
	tr.NodeUp("b", "r", time.Now())
	tr.NodeUp("c", "r", time.Now())
	tr.NodeUp("d", "r", time.Now()) // evicts "a"'s event
	h := tr.History(0)
	if len(h) != 3 {
		t.Fatalf("history cap not enforced: %d", len(h))
	}
	if h[0].NodeID != "d" {
		t.Fatalf("history should be newest-first; got %s", h[0].NodeID)
	}
	// limit
	if got := tr.History(1); len(got) != 1 || got[0].NodeID != "d" {
		t.Fatalf("History(1) = %+v", got)
	}
}

// helpers

func findSession(t *testing.T, tr *Tracker, id string) SessionStat {
	t.Helper()
	for _, s := range tr.Sessions() {
		if s.SessionID == id {
			return s
		}
	}
	t.Fatalf("session %s not found", id)
	return SessionStat{}
}

func countKind(tr *Tracker, kind string) int {
	n := 0
	for _, e := range tr.History(0) {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
