// SPDX-License-Identifier: GPL-3.0-or-later

package quicx_test

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/sourjatilak/revquic/internal/pki"
	"github.com/sourjatilak/revquic/internal/quicx"
)

func TestMTLSRawHandshake(t *testing.T) {
	caCert, caKey, _ := pki.GenerateCA("CA", time.Hour)
	srvCert, srvKey, _ := pki.IssueLeaf(caCert, caKey, "broker", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, true, false, time.Hour)
	cliCert, cliKey, _ := pki.IssueLeaf(caCert, caKey, "client", nil, nil, false, true, time.Hour)

	sc, _ := quicx.ServerMTLS(caCert, srvCert, srvKey)
	cc, _ := quicx.ClientMTLS(caCert, cliCert, cliKey, "localhost")

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	srv := tls.Server(c1, sc)
	cli := tls.Client(c2, cc)

	errc := make(chan error, 2)
	go func() { errc <- srv.Handshake() }()
	go func() { errc <- cli.Handshake() }()
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("handshake err: %v", err)
		}
	}
}
