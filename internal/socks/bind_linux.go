// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package socks

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// bindControl binds outbound sockets to ifName via SO_BINDTODEVICE (needs root / CAP_NET_RAW; the
// client already runs elevated).
func bindControl(ifName string) (func(network, address string, c syscall.RawConn) error, error) {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifName)
		}); err != nil {
			return err
		}
		return serr
	}, nil
}
