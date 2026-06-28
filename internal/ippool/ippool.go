// SPDX-License-Identifier: GPL-3.0-or-later

// Package ippool allocates and reclaims per-session VPN addresses from a CIDR. The exit's tunnel
// subnet (e.g. 10.99.0.0/24) hands each client session a unique /32; addresses are returned to the
// pool on session end so they can be reused (no monotonic leak).
package ippool

import (
	"errors"
	"net/netip"
	"sync"
)

// ErrExhausted is returned when no address is available.
var ErrExhausted = errors.New("ippool: address space exhausted")

// Pool allocates host addresses from a prefix, skipping the network, broadcast, and reserved hosts.
type Pool struct {
	mu       sync.Mutex
	prefix   netip.Prefix
	reserved map[netip.Addr]bool // never handed out (e.g. the gateway)
	used     map[netip.Addr]bool
	next     netip.Addr // rotating cursor for fairness
}

// New builds a pool over cidr (e.g. "10.99.0.0/24"). reserved addresses (such as the gateway) are
// excluded from allocation.
func New(cidr string, reserved ...netip.Addr) (*Pool, error) {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, err
	}
	pfx = pfx.Masked()
	p := &Pool{
		prefix:   pfx,
		reserved: map[netip.Addr]bool{},
		used:     map[netip.Addr]bool{},
		next:     pfx.Addr().Next(), // skip network address
	}
	for _, r := range reserved {
		p.reserved[r] = true
	}
	return p, nil
}

// usable reports whether addr is a host address inside the prefix that may be allocated.
func (p *Pool) usable(addr netip.Addr) bool {
	if !p.prefix.Contains(addr) {
		return false
	}
	if addr == p.prefix.Addr() {
		return false // network address
	}
	if p.isBroadcast(addr) {
		return false
	}
	return !p.reserved[addr]
}

// isBroadcast reports whether addr is the all-ones (broadcast) address of an IPv4 prefix.
func (p *Pool) isBroadcast(addr netip.Addr) bool {
	if !addr.Is4() {
		return false
	}
	bits := addr.As4()
	hostBits := 32 - p.prefix.Bits()
	// the broadcast address has all host bits set
	for i := 0; i < hostBits; i++ {
		byteIdx := 3 - i/8
		bitIdx := i % 8
		if bits[byteIdx]&(1<<bitIdx) == 0 {
			return false
		}
	}
	return true
}

// Allocate returns the next free host address, or ErrExhausted.
func (p *Pool) Allocate() (netip.Addr, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	addr := p.next
	for i := 0; ; i++ {
		if !addr.IsValid() || !p.prefix.Contains(addr) {
			addr = p.prefix.Addr().Next() // wrap to first host
		}
		if p.usable(addr) && !p.used[addr] {
			p.used[addr] = true
			p.next = addr.Next()
			return addr, nil
		}
		addr = addr.Next()
		// full sweep without finding a slot -> exhausted
		if i > 1<<(32-p.prefix.Bits())+1 {
			return netip.Addr{}, ErrExhausted
		}
	}
}

// Release returns an address to the pool.
func (p *Pool) Release(addr netip.Addr) {
	p.mu.Lock()
	delete(p.used, addr)
	p.mu.Unlock()
}

// InUse returns the number of allocated addresses.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}
