// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

// Package netcfg (windows) implements the Windows client data plane: it assigns the Wintun adapter
// address/MTU and programs the full-tunnel routes using netsh(8) and route(8). Requires
// Administrator. Exit-side NAT remains Linux-only.
package netcfg

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
)

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runAll(cmds [][]string) error {
	for _, c := range cmds {
		if err := run(c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// winState remembers the next-hop gateway derived in AddrUp so SetDefaultRoute can reuse it
// (single client process per run).
var winState struct {
	mu sync.Mutex
	gw string
}

// AddrUp assigns the adapter address and MTU.
func AddrUp(ifName, cidr string, mtu int) error {
	cmds, gw, err := winAddrUpCmds(ifName, cidr, mtu)
	if err != nil {
		return err
	}
	winState.mu.Lock()
	winState.gw = gw
	winState.mu.Unlock()
	return runAll(cmds)
}

// AddHostRoute pins a host route to the broker via the current default gateway. devName is unused
// on Windows (the route is keyed on the gateway IP).
func AddHostRoute(brokerIP, viaGateway, devName string) error {
	return runAll(winHostRouteCmds(brokerIP, viaGateway))
}

// SetDefaultRoute installs the split-default routes over the tunnel adapter.
func SetDefaultRoute(ifName string) error {
	winState.mu.Lock()
	gw := winState.gw
	winState.mu.Unlock()
	return runAll(winDefaultRouteCmds(ifName, gw))
}

// Cleanup best-effort removes the routes added by the client (the Wintun adapter itself is removed
// when the owning process exits). gw/gwDev are unused on Windows.
func Cleanup(ifName, brokerIP, gw, gwDev string, full bool) {
	for _, c := range winCleanupCmds(ifName, brokerIP, full) {
		_ = run(c[0], c[1:]...)
	}
}

// EnableForwarding turns on IPv4 forwarding (exit node).
func EnableForwarding() error { return runAll(winEnableForwardingCmds()) }

// Masquerade NATs the tunnel subnet out to the internet using WinNAT, then adds a Windows Defender
// Firewall allow rule for the subnet. uplinkIface is accepted for signature compatibility but
// ignored — WinNAT selects the external interface automatically. WinNAT is REQUIRED: if it is not
// present this returns an error and the exit stops (a Windows exit cannot NAT without it).
func Masquerade(srcCIDR, uplinkIface string) error {
	if err := runAll(winMasqueradeCmds(srcCIDR)); err != nil {
		return err // WinNAT missing/unavailable — caller (main) treats this as fatal
	}
	if err := runAll(winFirewallRuleCmds(srcCIDR)); err != nil {
		log.Printf("WARNING: firewall allow rule for %s could not be added (continuing): %v", srcCIDR, err)
	}
	return nil
}

// Isolate is a no-op on Windows: per-tenant isolation cannot be enforced. The Linux exit uses
// iptables FORWARD rules, but Windows Firewall has no "forward" direction and WinNAT exposes no
// equivalent, so client-to-client and client-to-LAN traffic CANNOT be filtered here. A Linux exit
// is required for real isolation.
func Isolate(vpnCIDR string) error {
	log.Printf("WARNING: -isolate has NO EFFECT on a Windows exit — WinNAT cannot enforce per-tenant " +
		"isolation (no client-to-client or client-to-LAN filtering). Use a Linux exit for isolation.")
	return nil
}
