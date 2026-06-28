// SPDX-License-Identifier: GPL-3.0-or-later

package lb

import "testing"

func nodes() []Node {
	return []Node{
		{ID: "a", Region: "us-west", Active: 5, Capacity: 100},
		{ID: "b", Region: "us-west", Active: 2, Capacity: 100},
		{ID: "c", Region: "eu-west", Active: 0, Capacity: 100},
		{ID: "d", Region: "us-west", Active: 99, Capacity: 100},
	}
}

func TestLeastConnPicksFewestActiveInRegion(t *testing.T) {
	got, ok := Pick(LeastConn, "us-west", nodes(), nil)
	if !ok || got != "b" {
		t.Fatalf("LeastConn = %q ok=%v, want b", got, ok)
	}
}

func TestLeastConnTieBreaksByLoadFractionThenID(t *testing.T) {
	ns := []Node{
		{ID: "y", Region: "r", Active: 2, Capacity: 10}, // 20%
		{ID: "x", Region: "r", Active: 2, Capacity: 4},  // 50%
		{ID: "z", Region: "r", Active: 2, Capacity: 10}, // 20%, ties y -> lower id "y"
	}
	got, ok := Pick(LeastConn, "r", ns, nil)
	if !ok || got != "y" {
		t.Fatalf("tie-break = %q, want y (lowest load frac, lowest id)", got)
	}
}

func TestCapacityFilterExcludesFull(t *testing.T) {
	ns := []Node{
		{ID: "full", Region: "r", Active: 10, Capacity: 10},
		{ID: "open", Region: "r", Active: 9, Capacity: 10},
	}
	got, ok := Pick(LeastConn, "r", ns, nil)
	if !ok || got != "open" {
		t.Fatalf("got %q, want open (full excluded)", got)
	}
}

func TestNoEligibleReturnsFalse(t *testing.T) {
	if _, ok := Pick(LeastConn, "ap-south", nodes(), nil); ok {
		t.Fatalf("expected no eligible exit in ap-south")
	}
	// All full.
	ns := []Node{{ID: "x", Region: "r", Active: 10, Capacity: 10}}
	if _, ok := Pick(LeastConn, "r", ns, nil); ok {
		t.Fatalf("expected false when all exits are at capacity")
	}
}

func TestUnlimitedCapacityIsEligible(t *testing.T) {
	ns := []Node{{ID: "x", Region: "r", Active: 9999, Capacity: 0}}
	got, ok := Pick(LeastConn, "r", ns, nil)
	if !ok || got != "x" {
		t.Fatalf("unlimited capacity should be eligible; got %q ok=%v", got, ok)
	}
}

func TestRoundRobinCyclesEligibleInIDOrder(t *testing.T) {
	ns := []Node{
		{ID: "a", Region: "r", Active: 0, Capacity: 10},
		{ID: "b", Region: "r", Active: 0, Capacity: 10},
		{ID: "c", Region: "r", Active: 0, Capacity: 10},
	}
	var rr uint64
	want := []string{"a", "b", "c", "a", "b"}
	for i, w := range want {
		got, ok := Pick(RoundRobin, "r", ns, &rr)
		if !ok || got != w {
			t.Fatalf("rr step %d = %q, want %q", i, got, w)
		}
	}
}

func TestParseDefaultsToLeastConn(t *testing.T) {
	if Parse("") != LeastConn || Parse("bogus") != LeastConn {
		t.Fatalf("Parse should default to LeastConn")
	}
	if Parse("round-robin") != RoundRobin || Parse("random") != Random {
		t.Fatalf("Parse known strategies failed")
	}
}
