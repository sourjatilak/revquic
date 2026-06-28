// SPDX-License-Identifier: GPL-3.0-or-later

package socks

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestNegotiateAndRequest drives the SOCKS5 greeting + CONNECT request parsing over a pipe.
// net.Pipe is synchronous, so the client side must read the method-selection reply before sending
// the request (mirroring a real SOCKS5 exchange).
func TestNegotiateAndRequest(t *testing.T) {
	s := &Server{logf: func(string, ...any) {}}
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	_ = cli.SetDeadline(time.Now().Add(2 * time.Second))
	_ = srv.SetDeadline(time.Now().Add(2 * time.Second))

	go func() {
		// Greeting: VER=5, 1 method, NO-AUTH.
		cli.Write([]byte{ver5, 1, methodNoAuth})
		// Read the server's method-selection reply.
		reply := make([]byte, 2)
		io.ReadFull(cli, reply)
		// Request: CONNECT example.com:443 (domain).
		host := "example.com"
		req := []byte{ver5, cmdConnect, 0x00, atypDomain, byte(len(host))}
		req = append(req, []byte(host)...)
		req = append(req, 0x01, 0xBB) // port 443
		cli.Write(req)
	}()

	if err := s.negotiateAuth(srv); err != nil {
		t.Fatalf("negotiateAuth: %v", err)
	}
	host, port, err := readRequest(srv)
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if host != "example.com" || port != "443" {
		t.Fatalf("readRequest = (%q,%q), want (example.com,443)", host, port)
	}
}

func TestReadRequestIPv4(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	_ = cli.SetDeadline(time.Now().Add(2 * time.Second))
	_ = srv.SetDeadline(time.Now().Add(2 * time.Second))
	go func() {
		// CONNECT 1.2.3.4:80
		cli.Write([]byte{ver5, cmdConnect, 0x00, atypIPv4, 1, 2, 3, 4, 0x00, 0x50})
	}()
	host, port, err := readRequest(srv)
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	if host != "1.2.3.4" || port != "80" {
		t.Fatalf("readRequest = (%q,%q), want (1.2.3.4,80)", host, port)
	}
}

func TestReadRequestRejectsBind(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	_ = cli.SetDeadline(time.Now().Add(2 * time.Second))
	_ = srv.SetDeadline(time.Now().Add(2 * time.Second))
	go func() { cli.Write([]byte{ver5, 0x02 /* BIND */, 0x00, atypIPv4, 1, 2, 3, 4, 0, 80}) }()
	if _, _, err := readRequest(srv); err != errCmdNotSupported {
		t.Fatalf("readRequest(BIND) err = %v, want errCmdNotSupported", err)
	}
}
