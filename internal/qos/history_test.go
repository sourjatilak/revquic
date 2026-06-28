// SPDX-License-Identifier: GPL-3.0-or-later

package qos

import (
	"path/filepath"
	"testing"
	"time"
)

func sampleEvents() []Event {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []Event{
		{TS: base, Kind: KindNodeUp, NodeID: "exit-1", Region: "us-west"},
		{TS: base.Add(time.Second), Kind: KindConnect, SessionID: "1", NodeID: "exit-1", Region: "us-west", Username: "alice"},
		{TS: base.Add(2 * time.Second), Kind: KindSpeedDrop, SessionID: "1", NodeID: "exit-1", ThroughputBps: 1234.5},
		{TS: base.Add(3 * time.Second), Kind: KindDisconnect, SessionID: "1", NodeID: "exit-1"},
	}
}

// roundTrip appends events to a store, closes it, reopens via opener, and returns the loaded events.
func roundTrip(t *testing.T, open func() (HistoryStore, error)) []Event {
	t.Helper()
	st, err := open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, ev := range sampleEvents() {
		st.Append(ev)
	}
	if err := st.Close(); err != nil { // Close drains the async queue
		t.Fatalf("close: %v", err)
	}
	st2, err := open()
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	got, err := st2.Load(0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return got
}

func TestFileHistoryRoundTripNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qos.jsonl")
	got := roundTrip(t, func() (HistoryStore, error) { return NewFileHistory(path) })
	if len(got) != 4 {
		t.Fatalf("loaded %d events, want 4", len(got))
	}
	if got[0].Kind != KindDisconnect { // newest first
		t.Fatalf("newest-first ordering broken: %s", got[0].Kind)
	}
	if got[2].Kind != KindConnect || got[2].Username != "alice" {
		t.Fatalf("fields not persisted: %+v", got[2])
	}
}

func TestSQLiteHistoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qos.db")
	got := roundTrip(t, func() (HistoryStore, error) { return NewSQLiteHistory(path) })
	if len(got) != 4 {
		t.Fatalf("loaded %d events, want 4", len(got))
	}
	if got[0].Kind != KindDisconnect {
		t.Fatalf("newest-first ordering broken: %s", got[0].Kind)
	}
	if got[1].Kind != KindSpeedDrop || got[1].ThroughputBps != 1234.5 {
		t.Fatalf("throughput not persisted: %+v", got[1])
	}
}

func TestCloseIdempotentAndAppendAfterCloseSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qos.jsonl")
	st, err := NewFileHistory(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Append(sampleEvents()[0])
	if err := st.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := st.Close(); err != nil { // idempotent
		t.Fatalf("close 2: %v", err)
	}
	// Append after Close must be a safe no-op (no panic on the closed channel).
	st.Append(sampleEvents()[1])
}

func TestTrackerPersistsAndPreloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qos.jsonl")

	// First tracker: generate some history into the store.
	st, err := NewFileHistory(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tr := New(100).WithStore(st)
	tr.NodeUp("exit-1", "us-west", time.Now())
	tr.Connect("1", "exit-1", "us-west", "alice", time.Now())
	tr.Disconnect("1")
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Second tracker: preload from the same store and confirm the ring is restored.
	st2, err := NewFileHistory(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	tr2 := New(100).WithStore(st2)
	if err := tr2.Preload(100); err != nil {
		t.Fatalf("preload: %v", err)
	}
	h := tr2.History(0)
	if len(h) != 3 {
		t.Fatalf("preloaded %d events, want 3", len(h))
	}
	if h[0].Kind != KindDisconnect { // newest first
		t.Fatalf("preload ordering broken: %s", h[0].Kind)
	}
}
