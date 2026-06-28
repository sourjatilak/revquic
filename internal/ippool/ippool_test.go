// SPDX-License-Identifier: GPL-3.0-or-later

package ippool

import (
	"net/netip"
	"testing"
)

func mustAddr(s string) netip.Addr { a, _ := netip.ParseAddr(s); return a }

func TestAllocateDistinctAndReserved(t *testing.T) {
	p, err := New("10.99.0.0/24", mustAddr("10.99.0.1")) // reserve the gateway
	if err != nil {
		t.Fatal(err)
	}
	seen := map[netip.Addr]bool{}
	for i := 0; i < 10; i++ {
		a, err := p.Allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if a == mustAddr("10.99.0.1") {
			t.Fatal("allocated the reserved gateway")
		}
		if a == mustAddr("10.99.0.0") || a == mustAddr("10.99.0.255") {
			t.Fatalf("allocated network/broadcast: %s", a)
		}
		if seen[a] {
			t.Fatalf("duplicate allocation %s", a)
		}
		seen[a] = true
	}
}

func TestReleaseReuses(t *testing.T) {
	p, _ := New("10.99.0.0/30", mustAddr("10.99.0.1")) // /30 -> hosts .1 (reserved), .2 ; .0 net, .3 bcast
	a, err := p.Allocate()                             // -> 10.99.0.2
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if a != mustAddr("10.99.0.2") {
		t.Fatalf("got %s want 10.99.0.2", a)
	}
	if _, err := p.Allocate(); err != ErrExhausted {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
	p.Release(a)
	if p.InUse() != 0 {
		t.Fatalf("InUse=%d after release", p.InUse())
	}
	b, err := p.Allocate()
	if err != nil || b != a {
		t.Fatalf("reuse: got %s err=%v, want %s", b, err, a)
	}
}

func TestExhaustion(t *testing.T) {
	p, _ := New("10.99.0.0/29", mustAddr("10.99.0.1")) // hosts .1(reserved)..6 ; .0 net .7 bcast => 5 usable
	n := 0
	for {
		if _, err := p.Allocate(); err != nil {
			break
		}
		n++
		if n > 100 {
			t.Fatal("did not exhaust")
		}
	}
	if n != 5 {
		t.Fatalf("usable count = %d, want 5", n)
	}
}
