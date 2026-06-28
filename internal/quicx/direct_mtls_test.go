// SPDX-License-Identifier: GPL-3.0-or-later

package quicx_test

import (
	"context"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/pki"
	"github.com/sourjatilak/revquic/internal/quicx"
)

// TestDirectMTLSNoSNI verifies the direct-path mTLS: the exit (server) presents a CA-signed
// node cert (server+client EKU) and requires a client cert; the client verifies the server's cert
// chains to the CA WITHOUT hostname matching (ClientMTLSNoSNI), since the address is dynamic.
func TestDirectMTLSNoSNI(t *testing.T) {
	caCert, caKey, _ := pki.GenerateCA("CA", time.Hour)
	// node cert: BOTH server and client EKU (exit is the direct-path TLS server)
	nodeCert, nodeKey, _ := pki.IssueLeaf(caCert, caKey, "exit-1", nil, nil, true, true, time.Hour)
	cliCert, cliKey, _ := pki.IssueLeaf(caCert, caKey, "client", nil, nil, false, true, time.Hour)

	serverTLS, err := quicx.ServerMTLS(caCert, nodeCert, nodeKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLS, quicx.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan quic.Connection, 1)
	go func() {
		for {
			c, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	// client dials 127.0.0.1 (no SAN match) with no-SNI verify -> should still succeed (CA chain).
	clientTLS, err := quicx.ClientMTLSNoSNI(caCert, cliCert, cliKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, ln.Addr().String(), clientTLS, quicx.Config())
	if err != nil {
		t.Fatalf("no-SNI mTLS dial failed: %v", err)
	}
	select {
	case sc := <-accepted:
		if len(sc.ConnectionState().TLS.PeerCertificates) == 0 {
			t.Fatal("server saw no client cert")
		}
		_ = sc.CloseWithError(0, "")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept the no-SNI mTLS connection")
	}
	_ = conn.CloseWithError(0, "done")

	// wrong CA on the client -> server cert verification must fail.
	otherCA, _, _ := pki.GenerateCA("OtherCA", time.Hour)
	badClientTLS, err := quicx.ClientMTLSNoSNI(otherCA, cliCert, cliKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if bad, err := quic.DialAddr(ctx2, ln.Addr().String(), badClientTLS, quicx.Config()); err == nil {
		_ = bad.CloseWithError(0, "")
		t.Fatal("dial with wrong CA unexpectedly succeeded")
	}
}
