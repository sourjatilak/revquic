// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

// Package netcfg configures interface addresses, routes, and NAT for the Phase 0 spike.
// Linux implementation via the `ip` and `iptables` binaries (must run as root).
package netcfg

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %v: %s", name, args, err, string(out))
	}
	return nil
}

// AddrUp assigns an address (CIDR, e.g. 10.99.0.2/24), sets MTU, and brings the link up.
func AddrUp(ifName, cidr string, mtu int) error {
	if err := run("ip", "addr", "add", cidr, "dev", ifName); err != nil {
		return err
	}
	if err := run("ip", "link", "set", "dev", ifName, "mtu", strconv.Itoa(mtu)); err != nil {
		return err
	}
	return run("ip", "link", "set", "dev", ifName, "up")
}

// AddHostRoute pins a /32 route to the broker via the current default gateway, so the
// QUIC tunnel to the broker does not recurse into the TUN when the default route is replaced.
func AddHostRoute(brokerIP, viaGateway, devName string) error {
	return run("ip", "route", "add", brokerIP+"/32", "via", viaGateway, "dev", devName)
}

// SetDefaultRoute replaces the default route to point at the TUN (full-tunnel).
func SetDefaultRoute(ifName string) error {
	return run("ip", "route", "replace", "default", "dev", ifName)
}

// Cleanup best-effort reverses AddHostRoute/SetDefaultRoute. Critically, because SetDefaultRoute
// REPLACED the original default (it isn't interface-scoped restore), we must put it back via the
// original gateway/device — otherwise after the tun goes away there is NO default route and the host
// loses connectivity. Errors are ignored (routes may already be gone with the tun).
func Cleanup(ifName, brokerIP, gw, gwDev string, full bool) {
	if brokerIP != "" {
		_ = run("ip", "route", "del", brokerIP+"/32")
	}
	if full {
		if gw != "" && gwDev != "" {
			_ = run("ip", "route", "replace", "default", "via", gw, "dev", gwDev)
		} else {
			_ = run("ip", "route", "del", "default", "dev", ifName)
		}
	}
}

// EnableForwarding turns on IPv4 forwarding (exit node). Idempotent: if forwarding is already on
// (e.g. set via container sysctls), it skips the write so it works without privileged on a
// read-only /proc/sys.
func EnableForwarding() error {
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	return run("sysctl", "-w", "net.ipv4.ip_forward=1")
}

// Masquerade NATs the tunnel subnet out the uplink interface (exit node).
func Masquerade(srcCIDR, uplinkIface string) error {
	if err := run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", srcCIDR, "-o", uplinkIface, "-j", "MASQUERADE"); err != nil {
		return err
	}
	if err := run("iptables", "-A", "FORWARD", "-o", uplinkIface, "-j", "ACCEPT"); err != nil {
		return err
	}
	return run("iptables", "-A", "FORWARD", "-i", uplinkIface, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")
}

// Isolate enforces per-session tenant isolation on the exit by inserting FORWARD DROP rules ABOVE
// the masquerade ACCEPT: (1) clients in vpnCIDR cannot reach each other, and (2) clients cannot
// reach private/LAN ranges (only public internet egress). Inbound replies (RELATED,ESTABLISHED) are
// unaffected. Traffic to the exit's own tunnel gateway is local (INPUT), so it is not blocked here.
func Isolate(vpnCIDR string) error {
	// inter-tenant: no client-to-client traffic
	if err := run("iptables", "-I", "FORWARD", "-s", vpnCIDR, "-d", vpnCIDR, "-j", "DROP"); err != nil {
		return err
	}
	// LAN/private protection: clients may only egress to public addresses
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} {
		if err := run("iptables", "-I", "FORWARD", "-s", vpnCIDR, "-d", cidr, "-j", "DROP"); err != nil {
			return err
		}
	}
	return nil
}
