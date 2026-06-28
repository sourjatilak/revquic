// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !windows

package socks

import (
	"fmt"
	"syscall"
)

// bindControl is unsupported on this platform.
func bindControl(ifName string) (func(network, address string, c syscall.RawConn) error, error) {
	return nil, fmt.Errorf("socks: per-interface binding is not supported on this platform")
}
