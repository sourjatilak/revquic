// SPDX-License-Identifier: GPL-3.0-or-later

// Package pki provides a minimal certificate authority for Revquic mTLS: generate a CA and issue
// ECDSA leaf certificates for the broker (server) and for nodes/clients (client auth). All material
// is PEM-encoded. This is enough for a self-hosted deployment where the broker operator runs the CA;
// for larger fleets, swap in a real CA / SPIFFE without changing the quicx mTLS config consumers.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func marshalKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// GenerateCA returns a self-signed CA certificate and its private key (PEM).
func GenerateCA(commonName string, validFor time.Duration) (caCertPEM, caKeyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Revquic"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM(der), keyPEM, nil
}

// IssueLeaf issues a leaf certificate signed by the CA. server/client select the extended key
// usages; dnsNames/ipSANs populate the SANs (use server certs' SANs for hostname verification).
func IssueLeaf(caCertPEM, caKeyPEM []byte, commonName string, dnsNames []string, ipSANs []net.IP, server, client bool, validFor time.Duration) (leafCertPEM, leafKeyPEM []byte, err error) {
	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	var eku []x509.ExtKeyUsage
	if server {
		eku = append(eku, x509.ExtKeyUsageServerAuth)
	}
	if client {
		eku = append(eku, x509.ExtKeyUsageClientAuth)
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"Revquic"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(validFor),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
		DNSNames:     dnsNames,
		IPAddresses:  ipSANs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM(der), keyPEM, nil
}

func parseCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(caCertPEM)
	if cb == nil {
		return nil, nil, fmt.Errorf("pki: bad CA cert PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := pem.Decode(caKeyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("pki: bad CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}
