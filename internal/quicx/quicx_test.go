// SPDX-License-Identifier: GPL-3.0-or-later

package quicx_test

import (
	"context"
	"net"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/pki"
	"github.com/sourjatilak/revquic/internal/quicx"
)

// TestQUICmTLS stands up a real QUIC listener with mTLS and verifies: (1) a client presenting a
// CA-signed cert connects and the server sees its client certificate; (2) a client without a client
// certificate is rejected (dial fails or the server never accepts the connection).
func TestQUICmTLS(t *testing.T) {
	caCert, caKey, err := pki.GenerateCA("Revquic Test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srvCert, srvKey, err := pki.IssueLeaf(caCert, caKey, "broker", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, true, false, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cliCert, cliKey, err := pki.IssueLeaf(caCert, caKey, "client", nil, nil, false, true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	serverTLS, err := quicx.ServerMTLS(caCert, srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLS, quicx.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	accepted := make(chan quic.Connection, 2)
	go func() {
		for {
			c, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	// 1) valid client cert -> connects; server sees the client certificate (mutual auth).
	clientTLS, err := quicx.ClientMTLS(caCert, cliCert, cliKey, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, addr, clientTLS, quicx.Config())
	if err != nil {
		t.Fatalf("mTLS dial with valid cert failed: %v", err)
	}
	select {
	case sc := <-accepted:
		if certs := sc.ConnectionState().TLS.PeerCertificates; len(certs) == 0 {
			t.Fatal("server accepted connection but saw no client certificate")
		} else if certs[0].Subject.CommonName != "client" {
			t.Fatalf("unexpected client cert CN %q", certs[0].Subject.CommonName)
		}
		_ = sc.CloseWithError(0, "")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept the valid mTLS connection")
	}
	_ = conn.CloseWithError(0, "done")

	// 2) no client cert -> dial fails, or the server never accepts the connection.
	noCert := quicx.ClientTLS() // InsecureSkipVerify, presents NO client certificate
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	bad, err := quic.DialAddr(ctx2, addr, noCert, quicx.Config())
	if err != nil {
		return // rejected at handshake — correct
	}
	select {
	case <-accepted:
		t.Fatal("server accepted a connection without a client certificate; mTLS not enforced")
	case <-time.After(1500 * time.Millisecond):
		// server never surfaced it — correct
	}
	_ = bad.CloseWithError(0, "")
}
