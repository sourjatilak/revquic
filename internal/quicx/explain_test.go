// SPDX-License-Identifier: GPL-3.0-or-later

package quicx

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestExplain(t *testing.T) {
	const addr = "broker.example.com:4242"

	cases := []struct {
		name string
		err  error
		want string // substring that must appear
	}{
		{"nil", nil, ""},
		{"idle-timeout", errors.New("timeout: no recent network activity"), "no response from broker"},
		{"deadline", context.DeadlineExceeded, "no response from broker"},
		{"dns", &net.DNSError{Err: "no such host", Name: "broker.example.com"}, "cannot resolve broker host"},
		{"refused", errors.New("dial udp: connect: connection refused"), "refused the connection"},
		{"unreachable", errors.New("network is unreachable"), "network unreachable"},
		{"tls", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), "TLS handshake"},
		{"crypto", errors.New("CRYPTO_ERROR 0x12a (remote)"), "TLS handshake"},
		{"unknown", errors.New("some weird error"), "could not reach broker"},
	}
	for _, c := range cases {
		got := Explain(c.err, addr)
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: want empty, got %q", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: Explain(%v) = %q, want substring %q", c.name, c.err, got, c.want)
		}
		// The unhelpful raw phrase must never be the whole message.
		if got == "timeout: no recent network activity" {
			t.Errorf("%s: leaked the raw quic-go message", c.name)
		}
	}
}

func TestHostPort(t *testing.T) {
	cases := []struct{ in, host, port string }{
		{"broker:4242", "broker", "4242"},
		{"1.2.3.4:9000", "1.2.3.4", "9000"},
		{"broker", "broker", "4242"}, // no port -> default
		{"", "(unset)", "4242"},
	}
	for _, c := range cases {
		h, p := hostPort(c.in)
		if h != c.host || p != c.port {
			t.Errorf("hostPort(%q) = (%q,%q), want (%q,%q)", c.in, h, p, c.host, c.port)
		}
	}
}
