// SPDX-License-Identifier: GPL-3.0-or-later

package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"
)

func parseCert(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(p)
	if b == nil {
		t.Fatal("bad cert PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func TestGenerateCAAndIssueLeaf(t *testing.T) {
	caCert, caKey, err := GenerateCA("Revquic Test CA", time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	ca := parseCert(t, caCert)
	if !ca.IsCA {
		t.Fatal("CA cert IsCA=false")
	}

	srvCert, _, err := IssueLeaf(caCert, caKey, "broker", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, true, false, time.Hour)
	if err != nil {
		t.Fatalf("IssueLeaf server: %v", err)
	}
	cliCert, _, err := IssueLeaf(caCert, caKey, "exit-1", nil, nil, false, true, time.Hour)
	if err != nil {
		t.Fatalf("IssueLeaf client: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caCert)

	// server cert verifies for ServerAuth + hostname
	srv := parseCert(t, srvCert)
	if _, err := srv.Verify(x509.VerifyOptions{Roots: roots, DNSName: "localhost", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("server cert verify: %v", err)
	}
	// client cert verifies for ClientAuth
	cli := parseCert(t, cliCert)
	if _, err := cli.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client cert verify: %v", err)
	}
	// client cert must NOT satisfy ServerAuth
	if _, err := cli.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("client cert unexpectedly valid for ServerAuth")
	}
}

func TestLeafFromWrongCARejected(t *testing.T) {
	caCert, caKey, _ := GenerateCA("CA1", time.Hour)
	otherCA, _, _ := GenerateCA("CA2", time.Hour)
	leaf, _, err := IssueLeaf(caCert, caKey, "node", nil, nil, false, true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(otherCA) // trust the WRONG CA
	if _, err := parseCert(t, leaf).Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Error("leaf verified against wrong CA")
	}
}
