// SPDX-License-Identifier: GPL-3.0-or-later

//go:build darwin

package netcfg

import (
	"net"
	"reflect"
	"testing"
)

func TestGatewayFirstHost(t *testing.T) {
	cases := map[string]string{
		"10.99.0.2/24":   "10.99.0.1",
		"10.99.0.50/24":  "10.99.0.1",
		"192.168.8.7/24": "192.168.8.1",
	}
	for cidr, want := range cases {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("parse %s: %v", cidr, err)
		}
		if got := gateway(ipnet).String(); got != want {
			t.Errorf("gateway(%s)=%s want %s", cidr, got, want)
		}
	}
}

func TestAddrUpCmds(t *testing.T) {
	cmds, err := addrUpCmds("utun4", "10.99.0.2/24", 1350)
	if err != nil {
		t.Fatalf("addrUpCmds: %v", err)
	}
	want := [][]string{
		{"ifconfig", "utun4", "inet", "10.99.0.2", "10.99.0.1", "up"},
		{"ifconfig", "utun4", "mtu", "1350"},
		{"route", "-n", "add", "-net", "10.99.0.0/24", "-interface", "utun4"},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("addrUpCmds =\n%v\nwant\n%v", cmds, want)
	}
}

func TestAddrUpCmdsNoMTU(t *testing.T) {
	cmds, _ := addrUpCmds("utun4", "10.99.0.2/24", 0)
	if len(cmds) != 2 { // no mtu command when mtu<=0
		t.Fatalf("expected 2 commands without mtu, got %d: %v", len(cmds), cmds)
	}
}

func TestSetDefaultRouteCmds(t *testing.T) {
	want := [][]string{
		{"route", "-n", "add", "-net", "0.0.0.0/1", "-interface", "utun4"},
		{"route", "-n", "add", "-net", "128.0.0.0/1", "-interface", "utun4"},
	}
	if got := setDefaultRouteCmds("utun4"); !reflect.DeepEqual(got, want) {
		t.Fatalf("setDefaultRouteCmds = %v want %v", got, want)
	}
}

func TestAddHostRouteCmds(t *testing.T) {
	want := [][]string{{"route", "-n", "add", "-host", "203.0.113.5", "192.168.1.1"}}
	if got := addHostRouteCmds("203.0.113.5", "192.168.1.1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("addHostRouteCmds = %v want %v", got, want)
	}
}
