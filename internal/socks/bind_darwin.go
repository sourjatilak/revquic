// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package socks

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindControl binds outbound sockets to the interface named ifName via IP_BOUND_IF (and
// IPV6_BOUND_IF), which forces egress out that interface regardless of the default route.
func bindControl(ifName string) (func(network, address string, c syscall.RawConn) error, error) {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("socks: interface %q: %w", ifName, err)
	}
	idx := ifi.Index
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			if e := unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, idx); e != nil {
				serr = e
				return
			}
			// Best-effort for IPv6 sockets; ignore errors on IPv4-only sockets.
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, idx)
		}); err != nil {
			return err
		}
		return serr
	}, nil
}
