// SPDX-License-Identifier: GPL-3.0-or-later

// No build tag: runs on any host so the Windows command construction is verifiable without Windows.
package netcfg

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestWinGateway(t *testing.T) {
	cases := map[string]string{
		"10.99.0.2/24":   "10.99.0.1",
		"10.99.0.77/24":  "10.99.0.1",
		"192.168.5.9/24": "192.168.5.1",
	}
	for cidr, want := range cases {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("parse %s: %v", cidr, err)
		}
		if got := winGateway(ipnet).String(); got != want {
			t.Errorf("winGateway(%s)=%s want %s", cidr, got, want)
		}
	}
}

func TestWinAddrUpCmds(t *testing.T) {
	cmds, gw, err := winAddrUpCmds("Revquic", "10.99.0.2/24", 1350)
	if err != nil {
		t.Fatalf("winAddrUpCmds: %v", err)
	}
	if gw != "10.99.0.1" {
		t.Errorf("gw=%s want 10.99.0.1", gw)
	}
	want := [][]string{
		{"netsh", "interface", "ip", "set", "address", "name=Revquic", "source=static", "addr=10.99.0.2", "mask=255.255.255.0"},
		{"netsh", "interface", "ipv4", "set", "subinterface", "Revquic", "mtu=1350", "store=active"},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("winAddrUpCmds =\n%v\nwant\n%v", cmds, want)
	}
}

func TestWinAddrUpCmdsNoMTU(t *testing.T) {
	cmds, _, _ := winAddrUpCmds("Revquic", "10.99.0.2/24", 0)
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command without mtu, got %d: %v", len(cmds), cmds)
	}
}

func TestWinDefaultRouteCmds(t *testing.T) {
	want := [][]string{
		{"netsh", "interface", "ipv4", "add", "route", "prefix=0.0.0.0/1", "interface=Revquic", "nexthop=10.99.0.1"},
		{"netsh", "interface", "ipv4", "add", "route", "prefix=128.0.0.0/1", "interface=Revquic", "nexthop=10.99.0.1"},
	}
	if got := winDefaultRouteCmds("Revquic", "10.99.0.1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("winDefaultRouteCmds = %v want %v", got, want)
	}
}

func TestWinHostRouteCmds(t *testing.T) {
	want := [][]string{{"route", "add", "203.0.113.5", "mask", "255.255.255.255", "192.168.1.1", "metric", "1"}}
	if got := winHostRouteCmds("203.0.113.5", "192.168.1.1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("winHostRouteCmds = %v want %v", got, want)
	}
}

func TestWinCleanupCmds(t *testing.T) {
	full := winCleanupCmds("Revquic", "203.0.113.5", true)
	want := [][]string{
		{"netsh", "interface", "ipv4", "delete", "route", "prefix=0.0.0.0/1", "interface=Revquic"},
		{"netsh", "interface", "ipv4", "delete", "route", "prefix=128.0.0.0/1", "interface=Revquic"},
		{"route", "delete", "203.0.113.5"},
	}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("winCleanupCmds(full) =\n%v\nwant\n%v", full, want)
	}
	// Not full + no broker IP => nothing to clean.
	if got := winCleanupCmds("Revquic", "", false); len(got) != 0 {
		t.Fatalf("winCleanupCmds(empty) = %v want []", got)
	}
}

func TestWinEnableForwardingCmds(t *testing.T) {
	want := [][]string{{"powershell", "-NoProfile", "-NonInteractive", "-Command", "Set-NetIPInterface -AddressFamily IPv4 -Forwarding Enabled"}}
	if got := winEnableForwardingCmds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("winEnableForwardingCmds = %v want %v", got, want)
	}
}

func TestWinMasqueradeCmds(t *testing.T) {
	got := winMasqueradeCmds("10.99.0.0/24")
	if len(got) != 1 || len(got[0]) != 5 || got[0][0] != "powershell" || got[0][3] != "-Command" {
		t.Fatalf("expected a single powershell -Command invocation, got %v", got)
	}
	script := got[0][4]
	for _, want := range []string{
		"Start-Service WinNat",
		"Remove-NetNat -Name Revquic",
		"New-NetNat -Name Revquic -InternalIPInterfaceAddressPrefix 10.99.0.0/24 -ErrorAction Stop",
		"WinNAT is required",
		"exit 1",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("masquerade script missing %q\nscript: %s", want, script)
		}
	}
}

func TestWinFirewallRuleCmds(t *testing.T) {
	got := winFirewallRuleCmds("10.99.0.0/24")
	if len(got) != 1 || got[0][0] != "powershell" || got[0][3] != "-Command" {
		t.Fatalf("expected a single powershell -Command invocation, got %v", got)
	}
	script := got[0][4]
	for _, want := range []string{
		"Remove-NetFirewallRule -DisplayName $n",
		"New-NetFirewallRule -DisplayName $n -Direction Inbound  -Action Allow -RemoteAddress 10.99.0.0/24",
		"New-NetFirewallRule -DisplayName $n -Direction Outbound -Action Allow -RemoteAddress 10.99.0.0/24",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("firewall script missing %q\nscript: %s", want, script)
		}
	}
}
