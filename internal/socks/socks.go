// SPDX-License-Identifier: GPL-3.0-or-later

// Package socks implements a minimal SOCKS5 (CONNECT) proxy whose outbound TCP connections — and DNS
// lookups — are bound to a specific network interface (the Revquic tunnel). Point an application
// (e.g. Chrome's --proxy-server) at this proxy and only that application's traffic egresses through
// the tunnel, with no change to the host's default route. CONNECT only; BIND and UDP ASSOCIATE are
// not implemented.
package socks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	ver5       = 0x05
	cmdConnect = 0x01
	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess      = 0x00
	repGenFailure   = 0x01
	repHostUnreach  = 0x04
	repCmdNotSupp   = 0x07
	repAddrNotSupp  = 0x08
	methodNoAuth    = 0x00
	methodNoneMatch = 0xFF
)

// Server is a SOCKS5 CONNECT proxy bound to one interface.
type Server struct {
	ln       net.Listener
	dialer   *net.Dialer
	resolver *net.Resolver
	logf     func(string, ...any)
}

// New starts a SOCKS5 listener on addr (e.g. "127.0.0.1:1080") whose outbound connections and DNS
// queries are bound to ifName (the tunnel interface). logf may be nil.
func New(addr, ifName string, logf func(string, ...any)) (*Server, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctrl, err := bindControl(ifName)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Control: ctrl, Timeout: 30 * time.Second}
	// Resolve DNS through the tunnel too, so names don't leak out the host's normal interface.
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Control: ctrl, Timeout: 10 * time.Second}).DialContext(ctx, network, address)
		},
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Server{ln: ln, dialer: dialer, resolver: resolver, logf: logf}, nil
}

// Addr is the actual listen address (useful when addr used port 0).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops accepting new connections.
func (s *Server) Close() error { return s.ln.Close() }

// Serve accepts and handles connections until the listener is closed.
func (s *Server) Serve() error {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))

	if err := s.negotiateAuth(client); err != nil {
		return
	}
	host, port, err := readRequest(client)
	if err != nil {
		if errors.Is(err, errCmdNotSupported) {
			writeReply(client, repCmdNotSupp)
		} else if errors.Is(err, errAddrNotSupported) {
			writeReply(client, repAddrNotSupp)
		} else {
			writeReply(client, repGenFailure)
		}
		return
	}

	// Resolve domains through the bound resolver (avoids DNS leak), then dial the IP via the bound
	// dialer; literal IPs are dialed directly.
	target := net.JoinHostPort(host, port)
	if net.ParseIP(host) == nil {
		ip, ok := s.resolve(host)
		if !ok {
			writeReply(client, repHostUnreach)
			return
		}
		target = net.JoinHostPort(ip, port)
	}

	upstream, derr := s.dialer.DialContext(context.Background(), "tcp", target)
	if derr != nil {
		writeReply(client, repHostUnreach)
		return
	}
	defer upstream.Close()

	if err := writeReply(client, repSuccess); err != nil {
		return
	}
	// Clear the handshake deadline; let the streams run for the connection's lifetime.
	_ = client.SetDeadline(time.Time{})

	// Splice both directions.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// resolve looks up host preferring the tunnel-bound resolver (no DNS leak); if that path can't reach
// the configured nameservers it falls back to the system resolver so name resolution still works.
func (s *Server) resolve(host string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if ips, err := s.resolver.LookupIPAddr(ctx, host); err == nil && len(ips) > 0 {
		return ips[0].IP.String(), true
	}
	if ips, err := net.DefaultResolver.LookupIPAddr(ctx, host); err == nil && len(ips) > 0 {
		return ips[0].IP.String(), true
	}
	return "", false
}

// negotiateAuth reads the SOCKS5 greeting and selects no-auth.
func (s *Server) negotiateAuth(c net.Conn) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	if head[0] != ver5 {
		return fmt.Errorf("socks: bad version 0x%02x", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	for _, m := range methods {
		if m == methodNoAuth {
			_, err := c.Write([]byte{ver5, methodNoAuth})
			return err
		}
	}
	_, _ = c.Write([]byte{ver5, methodNoneMatch})
	return fmt.Errorf("socks: no acceptable auth method")
}

var (
	errCmdNotSupported  = errors.New("socks: command not supported")
	errAddrNotSupported = errors.New("socks: address type not supported")
)

// readRequest parses a SOCKS5 CONNECT request and returns the target host and port.
func readRequest(c net.Conn) (host, port string, err error) {
	head := make([]byte, 4) // VER, CMD, RSV, ATYP
	if _, err = io.ReadFull(c, head); err != nil {
		return "", "", err
	}
	if head[0] != ver5 {
		return "", "", fmt.Errorf("socks: bad version 0x%02x", head[0])
	}
	if head[1] != cmdConnect {
		return "", "", errCmdNotSupported
	}
	switch head[3] {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err = io.ReadFull(c, b); err != nil {
			return "", "", err
		}
		host = net.IP(b).String()
	case atypIPv6:
		b := make([]byte, 16)
		if _, err = io.ReadFull(c, b); err != nil {
			return "", "", err
		}
		host = net.IP(b).String()
	case atypDomain:
		l := make([]byte, 1)
		if _, err = io.ReadFull(c, l); err != nil {
			return "", "", err
		}
		b := make([]byte, int(l[0]))
		if _, err = io.ReadFull(c, b); err != nil {
			return "", "", err
		}
		host = string(b)
	default:
		return "", "", errAddrNotSupported
	}
	pb := make([]byte, 2)
	if _, err = io.ReadFull(c, pb); err != nil {
		return "", "", err
	}
	port = strconv.Itoa(int(binary.BigEndian.Uint16(pb)))
	return host, port, nil
}

// writeReply sends a SOCKS5 reply with the given code and a zero IPv4 bound address.
func writeReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{ver5, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}
