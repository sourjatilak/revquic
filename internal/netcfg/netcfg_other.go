// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !windows

// Package netcfg stubs for platforms without a real data-plane implementation. Linux has full
// support (netcfg_linux.go), and macOS (netcfg_darwin.go) and Windows (netcfg_windows.go) have
// client-side support; everywhere else these return errors so the binaries compile but fail fast
// at runtime.
package netcfg

import (
	"fmt"
	"runtime"
)

func unsupported(op string) error {
	return fmt.Errorf("netcfg.%s: unsupported on %s (Phase 0 requires Linux)", op, runtime.GOOS)
}

func AddrUp(ifName, cidr string, mtu int) error               { return unsupported("AddrUp") }
func AddHostRoute(brokerIP, viaGateway, devName string) error { return unsupported("AddHostRoute") }
func SetDefaultRoute(ifName string) error                     { return unsupported("SetDefaultRoute") }
func EnableForwarding() error                                 { return unsupported("EnableForwarding") }
func Masquerade(srcCIDR, uplinkIface string) error            { return unsupported("Masquerade") }
func Isolate(vpnCIDR string) error                            { return unsupported("Isolate") }

// Cleanup is a no-op on unsupported platforms (no routes were installed).
func Cleanup(ifName, brokerIP, gw, gwDev string, full bool) {}
