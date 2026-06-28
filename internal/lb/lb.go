// SPDX-License-Identifier: GPL-3.0-or-later

// Package lb selects an exit node for a new client session among the exits serving a region.
// It is pure and dependency-free so the broker can unit-test selection independently of QUIC.
//
// Strategies:
//   - LeastConn (default): the eligible exit with the fewest active clients (ties broken by the
//     lower load fraction, then by node id) — spreads load and respects heterogeneous capacities.
//   - RoundRobin: cycle through eligible exits in id order using a caller-held counter.
//   - Random: a uniformly random eligible exit.
//
// An exit is "eligible" when it serves the requested region and has spare capacity
// (Capacity <= 0 means unlimited).
package lb

import (
	"math/rand"
	"sort"
)

// Strategy names a selection policy.
type Strategy string

// Selection strategies.
const (
	LeastConn  Strategy = "least-conn"
	RoundRobin Strategy = "round-robin"
	Random     Strategy = "random"
)

// Parse maps a string to a Strategy, defaulting to LeastConn for empty/unknown input.
func Parse(s string) Strategy {
	switch Strategy(s) {
	case RoundRobin:
		return RoundRobin
	case Random:
		return Random
	default:
		return LeastConn
	}
}

// Node is the minimal load view of one exit needed to choose between them.
type Node struct {
	ID       string
	Region   string
	Active   int
	Capacity int // <= 0 means unlimited
}

func (n Node) hasSpare() bool { return n.Capacity <= 0 || n.Active < n.Capacity }

func (n Node) loadFraction() float64 {
	if n.Capacity <= 0 {
		return float64(n.Active) / 1e9 // unlimited: effectively only the raw count matters
	}
	return float64(n.Active) / float64(n.Capacity)
}

// Pick chooses an exit id for region using strategy. rr is an optional caller-owned round-robin
// counter (may be nil for non-round-robin strategies); Pick increments it when used. The second
// return is false when no eligible exit exists.
func Pick(strategy Strategy, region string, nodes []Node, rr *uint64) (string, bool) {
	// Filter to eligible exits in the region, sorted by id for determinism.
	eligible := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Region == region && n.hasSpare() {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) == 0 {
		return "", false
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })

	switch strategy {
	case RoundRobin:
		var i uint64
		if rr != nil {
			i = *rr
			*rr++
		}
		return eligible[i%uint64(len(eligible))].ID, true
	case Random:
		return eligible[rand.Intn(len(eligible))].ID, true
	default: // LeastConn
		best := eligible[0]
		for _, n := range eligible[1:] {
			if n.Active < best.Active ||
				(n.Active == best.Active && n.loadFraction() < best.loadFraction()) {
				best = n
			}
		}
		return best.ID, true
	}
}
