// SPDX-License-Identifier: GPL-3.0-or-later

package quicx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Explain turns a broker dial/connection error into a human-friendly, actionable message.
// brokerAddr is the host:port the endpoint was dialing (included for context). It never returns the
// bare "timeout: no recent network activity" that quic-go produces — instead it explains the likely
// cause (broker down, UDP port blocked, DNS failure, TLS/mTLS rejection, no network route, …).
func Explain(err error, brokerAddr string) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	host, port := hostPort(brokerAddr)

	// DNS resolution failure (bad hostname, no DNS, no internet).
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf("cannot resolve broker host %q (%s) — check the address, your DNS, and that you have internet", host, dnsErr.Err)
	}

	// TLS / certificate problems (check these before the generic timeout bucket).
	if strings.Contains(raw, "x509") || strings.Contains(raw, "certificate") ||
		strings.Contains(raw, "tls:") || strings.Contains(raw, "CRYPTO_ERROR") {
		return fmt.Sprintf("TLS handshake with broker %s failed (%v) — check -tls-ca/-tls-cert/-tls-key, that -tls-server-name matches the broker certificate SAN, and that the broker requires/accepts your cert", brokerAddr, err)
	}

	// Explicit connection-level network errors.
	switch {
	case strings.Contains(raw, "connection refused"):
		return fmt.Sprintf("broker %s refused the connection — is the broker running and listening on UDP port %s?", brokerAddr, port)
	case strings.Contains(raw, "no route to host"), strings.Contains(raw, "network is unreachable"):
		return fmt.Sprintf("network unreachable while reaching broker %s — is your internet/VPN up?", brokerAddr)
	}

	// QUIC handshake/idle timeout, context deadline, or any net timeout: no UDP reply came back.
	// This is the common "port blocked / broker down / no route" case that surfaces as the unhelpful
	// "timeout: no recent network activity".
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) ||
		strings.Contains(raw, "no recent network activity") || strings.Contains(raw, "handshake") {
		return fmt.Sprintf("no response from broker %s — it may be down, UDP port %s may be blocked by a firewall, or there is no network route to it (Revquic uses QUIC over UDP, not TCP)", brokerAddr, port)
	}

	// Fallback: still clearer than a bare quic-go error string.
	return fmt.Sprintf("could not reach broker %s: %v", brokerAddr, err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// hostPort splits "host:port" defensively, defaulting the port to 4242 when absent/unparsable.
func hostPort(addr string) (host, port string) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil || p == "" {
		if addr == "" {
			return "(unset)", "4242"
		}
		return addr, "4242"
	}
	return h, p
}
