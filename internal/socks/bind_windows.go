// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package socks

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// IP_UNICAST_IF (IPv4) and IPV6_UNICAST_IF (IPv6) socket options — both 31. Defined locally because
// the pinned golang.org/x/sys/windows does not export them.
const (
	ipUnicastIF   = 31
	ipv6UnicastIF = 31
)

// bindControl binds outbound sockets to the interface named ifName via IP_UNICAST_IF. For IPv4 the
// interface index must be supplied in network byte order; IPv6 uses host byte order.
func bindControl(ifName string) (func(network, address string, c syscall.RawConn) error, error) {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("socks: interface %q: %w", ifName, err)
	}
	// htonl(index): write the index big-endian, read it back as host-endian.
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], uint32(ifi.Index))
	idxBE := int(binary.LittleEndian.Uint32(be[:]))
	idx := ifi.Index
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			h := windows.Handle(fd)
			if e := windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIF, idxBE); e != nil {
				serr = e
				return
			}
			_ = windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIF, idx)
		}); err != nil {
			return err
		}
		return serr
	}, nil
}
