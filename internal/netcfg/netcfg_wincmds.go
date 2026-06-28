// SPDX-License-Identifier: GPL-3.0-or-later

// Windows data-plane command builders. These are pure (no exec, no build tag) so they can be unit
// tested on any host; netcfg_windows.go executes them via netsh/route. Kept separate from the
// windows-tagged file precisely so the argv construction is verifiable without a Windows runtime.
package netcfg

import (
	"fmt"
	"net"
	"strconv"
)

// winGateway returns the conventional first-host (network+1) address of cidr — used as the on-link
// next hop for the full-tunnel routes (e.g. 10.99.0.0/24 -> 10.99.0.1).
func winGateway(ipnet *net.IPNet) net.IP {
	ip := ipnet.IP.To4()
	if ip == nil {
		return ipnet.IP
	}
	gw := make(net.IP, 4)
	copy(gw, ip)
	gw[3]++
	return gw
}

// winAddrUpCmds builds the netsh commands to assign the adapter address + MTU. It also returns the
// derived next-hop gateway so SetDefaultRoute can reuse it.
func winAddrUpCmds(ifName, cidr string, mtu int) ([][]string, string, error) {
	local, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, "", fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	mask := net.IP(ipnet.Mask).String() // dotted netmask, e.g. 255.255.255.0
	gw := winGateway(ipnet).String()
	cmds := [][]string{
		{"netsh", "interface", "ip", "set", "address", "name=" + ifName, "source=static", "addr=" + local.String(), "mask=" + mask},
	}
	if mtu > 0 {
		cmds = append(cmds, []string{"netsh", "interface", "ipv4", "set", "subinterface", ifName, "mtu=" + strconv.Itoa(mtu), "store=active"})
	}
	return cmds, gw, nil
}

// winHostRouteCmds pins a /32 host route to the broker via the current default gateway, so the
// control/relay connection survives the full-tunnel override.
func winHostRouteCmds(brokerIP, viaGateway string) [][]string {
	return [][]string{{"route", "add", brokerIP, "mask", "255.255.255.255", viaGateway, "metric", "1"}}
}

// winDefaultRouteCmds installs the 0.0.0.0/1 + 128.0.0.0/1 split-default routes over the tunnel
// adapter (overriding the system default without deleting it).
func winDefaultRouteCmds(ifName, gw string) [][]string {
	return [][]string{
		{"netsh", "interface", "ipv4", "add", "route", "prefix=0.0.0.0/1", "interface=" + ifName, "nexthop=" + gw},
		{"netsh", "interface", "ipv4", "add", "route", "prefix=128.0.0.0/1", "interface=" + ifName, "nexthop=" + gw},
	}
}

const winNatName = "Revquic"

// winCleanupCmds best-effort reverses winDefaultRouteCmds + winHostRouteCmds (client side).
func winCleanupCmds(ifName, brokerIP string, full bool) [][]string {
	var c [][]string
	if full {
		c = append(c, []string{"netsh", "interface", "ipv4", "delete", "route", "prefix=0.0.0.0/1", "interface=" + ifName})
		c = append(c, []string{"netsh", "interface", "ipv4", "delete", "route", "prefix=128.0.0.0/1", "interface=" + ifName})
	}
	if brokerIP != "" {
		c = append(c, []string{"route", "delete", brokerIP})
	}
	return c
}

func ps(cmd string) []string {
	return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", cmd}
}

// winEnableForwardingCmds enables IPv4 forwarding on all interfaces (exit node).
// winFirewallRuleCmds adds Windows Defender Firewall allow rules for the VPN subnet so WinNAT-
// forwarded client traffic is not blocked. Idempotent: it removes any prior rule of the same name
// first, then allows inbound + outbound traffic whose remote address is in the tunnel prefix. A
// failure here is non-fatal (the firewall may already permit it); the caller logs and continues.
func winFirewallRuleCmds(vpnCIDR string) [][]string {
	script := "$n='Revquic-VPN'; " +
		"Remove-NetFirewallRule -DisplayName $n -ErrorAction SilentlyContinue; " +
		"New-NetFirewallRule -DisplayName $n -Direction Inbound  -Action Allow -RemoteAddress " + vpnCIDR + " -Profile Any -ErrorAction Stop | Out-Null; " +
		"New-NetFirewallRule -DisplayName $n -Direction Outbound -Action Allow -RemoteAddress " + vpnCIDR + " -Profile Any -ErrorAction Stop | Out-Null"
	return [][]string{ps(script)}
}

func winEnableForwardingCmds() [][]string {
	return [][]string{ps("Set-NetIPInterface -AddressFamily IPv4 -Forwarding Enabled")}
}

// winMasqueradeCmds NATs the tunnel subnet out to the internet using WinNAT. The uplink interface is
// chosen automatically by WinNAT, so srcCIDR is the only input.
//
// It runs as one PowerShell script that: (1) starts the WinNat service (it's often stopped), (2)
// removes any stale Revquic NAT inside try/catch for idempotency, and (3) creates the NAT with
// -ErrorAction Stop. A failure here (notably HRESULT 0x80041010 / WBEM_E_INVALID_CLASS, which means
// the MSFT_NetNat class is not registered — typically Windows Home or a missing WinNat driver) is
// caught and re-thrown as an actionable message so the exit doesn't crash with a raw CIMException.
func winMasqueradeCmds(srcCIDR string) [][]string {
	script := "Start-Service WinNat -ErrorAction SilentlyContinue; " +
		"try { Remove-NetNat -Name " + winNatName + " -Confirm:$false -ErrorAction SilentlyContinue } catch {}; " +
		"try { New-NetNat -Name " + winNatName + " -InternalIPInterfaceAddressPrefix " + srcCIDR + " -ErrorAction Stop | Out-Null } " +
		"catch { Write-Error ('WinNAT is required for the Windows exit but is unavailable: ' + $_.Exception.Message + " +
		"' -- New-NetNat/WinNAT is missing on this Windows edition (e.g. Home) or the WinNat service/driver is not installed. " +
		"Install/enable WinNAT (Windows Pro/Enterprise/Server, or enable the Hyper-V/WinNat feature), or run the exit on Linux (WSL2, Docker, or a VM).'); exit 1 }"
	return [][]string{ps(script)}
}
