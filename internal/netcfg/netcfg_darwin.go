// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

// Package netcfg (darwin) implements the macOS client data plane: it configures the kernel utun
// interface, sets the MTU, brings it up, pins a host route to the broker, and performs the
// full-tunnel default-route override. It shells out to ifconfig(8) and route(8) and therefore
// requires root (sudo).
//
// macOS utun interfaces are point-to-point, so the interface is configured with a local address
// and a peer (the exit-side TUN gateway, conventionally the .1 host of the assigned subnet).
// Full-tunnel uses the standard "two /1 routes" trick (0.0.0.0/1 + 128.0.0.0/1) to override the
// default route without deleting it — the same approach OpenVPN's redirect-gateway uses.
//
// Exit-side NAT (EnableForwarding/Masquerade/Isolate) remains Linux-only and returns an error here;
// run the exit node on Linux.
package netcfg

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gateway returns the conventional first-host (network+1) address of cidr — the exit-side TUN
// gateway a point-to-point utun peers with (e.g. 10.99.0.0/24 -> 10.99.0.1).
func gateway(ipnet *net.IPNet) net.IP {
	ip := ipnet.IP.To4()
	if ip == nil {
		return ipnet.IP
	}
	gw := make(net.IP, 4)
	copy(gw, ip)
	gw[3]++
	return gw
}

// AddrUp configures the utun point-to-point interface (local + peer), sets the MTU, brings it up,
// and routes the assigned VPN subnet over it.
func AddrUp(ifName, cidr string, mtu int) error {
	cmds, err := addrUpCmds(ifName, cidr, mtu)
	if err != nil {
		return err
	}
	return runAll(cmds)
}

func addrUpCmds(ifName, cidr string, mtu int) ([][]string, error) {
	local, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	peer := gateway(ipnet)
	cmds := [][]string{
		{"ifconfig", ifName, "inet", local.String(), peer.String(), "up"},
	}
	if mtu > 0 {
		cmds = append(cmds, []string{"ifconfig", ifName, "mtu", strconv.Itoa(mtu)})
	}
	// Route the whole VPN subnet over the tunnel (the peer host route is added by ifconfig above).
	cmds = append(cmds, []string{"route", "-n", "add", "-net", ipnet.String(), "-interface", ifName})
	return cmds, nil
}

// AddHostRoute pins a host route to the broker via the current default gateway, so the control and
// relay connections survive the full-tunnel default-route override. devName is unused on macOS
// (the route is keyed on the gateway IP).
func AddHostRoute(brokerIP, viaGateway, devName string) error {
	return runAll(addHostRouteCmds(brokerIP, viaGateway))
}

func addHostRouteCmds(brokerIP, viaGateway string) [][]string {
	return [][]string{{"route", "-n", "add", "-host", brokerIP, viaGateway}}
}

// SetDefaultRoute redirects all traffic into the tunnel by installing 0.0.0.0/1 and 128.0.0.0/1
// routes over the interface; these are more specific than the existing default and override it
// without removing it (so it is restored automatically when the tunnel interface goes away).
func SetDefaultRoute(ifName string) error {
	return runAll(setDefaultRouteCmds(ifName))
}

func setDefaultRouteCmds(ifName string) [][]string {
	return [][]string{
		{"route", "-n", "add", "-net", "0.0.0.0/1", "-interface", ifName},
		{"route", "-n", "add", "-net", "128.0.0.0/1", "-interface", ifName},
	}
}

// Cleanup best-effort reverses AddrUp/AddHostRoute/SetDefaultRoute. On macOS the utun (and its
// -interface routes) is destroyed automatically when the owning fd closes, and the original default
// route returns once the 0/1+128/1 overrides are gone, so the main leak is the broker host route.
// All commands are best-effort (errors ignored) since some routes may already be gone. gw/gwDev are
// unused on macOS (the default route auto-restores).
func Cleanup(ifName, brokerIP, gw, gwDev string, full bool) {
	if full {
		_ = run("route", "-n", "delete", "-net", "0.0.0.0/1")
		_ = run("route", "-n", "delete", "-net", "128.0.0.0/1")
	}
	if brokerIP != "" {
		_ = run("route", "-n", "delete", "-host", brokerIP)
	}
}

func runAll(cmds [][]string) error {
	for _, c := range cmds {
		if err := run(c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// Exit-side NAT helpers are Linux-only.
func EnableForwarding() error                      { return unsupportedDarwin("EnableForwarding") }
func Masquerade(srcCIDR, uplinkIface string) error { return unsupportedDarwin("Masquerade") }
func Isolate(vpnCIDR string) error                 { return unsupportedDarwin("Isolate") }

func unsupportedDarwin(op string) error {
	return fmt.Errorf("netcfg.%s: exit-side NAT is Linux-only — run the exit node on Linux", op)
}
