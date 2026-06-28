// SPDX-License-Identifier: GPL-3.0-or-later

// Package quicx centralizes QUIC + TLS configuration for the Phase 0 spike.
//
// SECURITY: the spike uses a self-signed cert and InsecureSkipVerify on the client.
// Production MUST use mutual TLS with broker-issued node certs and verified client tokens
// (see spec/low-level-design.md §2 and reconciliation-and-validation.md §5 gap #1).
package quicx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/sourjatilak/revquic/internal/proto"
)

var errBadCA = errors.New("quicx: invalid CA PEM")
var errNoPeerCert = errors.New("quicx: peer presented no certificate")

// ServerMTLSFromFiles loads CA + leaf PEM files and builds a mutual-TLS server config.
func ServerMTLSFromFiles(caPath, certPath, keyPath string) (*tls.Config, error) {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	return ServerMTLS(ca, cert, key)
}

// ClientMTLSFromFiles loads CA + leaf PEM files and builds a mutual-TLS client config.
func ClientMTLSFromFiles(caPath, certPath, keyPath, serverName string) (*tls.Config, error) {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	return ClientMTLS(ca, cert, key, serverName)
}

// ClientMTLSNoSNI builds a mutual-TLS client config for the direct (P2P) data path, where the peer's
// address is dynamic (hole-punched) so hostname verification is meaningless. It still presents the
// client leaf AND cryptographically verifies that the server's certificate chains to the CA with the
// ServerAuth EKU — just not a hostname. This is the standard approach for mesh/P2P TLS.
func ClientMTLSNoSNI(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errBadCA
	}
	return &tls.Config{
		Certificates:          []tls.Certificate{leaf},
		InsecureSkipVerify:    true, // #nosec G402 -- hostname check disabled; VerifyPeerCertificate enforces the CA chain
		VerifyPeerCertificate: verifyChain(pool, x509.ExtKeyUsageServerAuth),
		NextProtos:            []string{proto.ALPN},
		MinVersion:            tls.VersionTLS13,
	}, nil
}

// ClientMTLSNoSNIFromFiles loads CA + leaf PEM files and builds a no-SNI mutual-TLS client config.
func ClientMTLSNoSNIFromFiles(caPath, certPath, keyPath string) (*tls.Config, error) {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	cert, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	return ClientMTLSNoSNI(ca, cert, key)
}

// verifyChain returns a VerifyPeerCertificate func that verifies the peer's leaf chains to roots
// with the given EKU (ignoring hostname). Used for the dynamic-address direct path.
func verifyChain(roots *x509.CertPool, eku x509.ExtKeyUsage) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errNoPeerCert
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return err
			}
			certs = append(certs, c)
		}
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		_, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{eku}})
		return err
	}
}

// Config returns a QUIC config with unreliable datagrams enabled (the Phase 0 data plane).
func Config() *quic.Config {
	return &quic.Config{
		EnableDatagrams: true,
		KeepAlivePeriod: 5 * time.Second,  // holds NAT bindings and keeps live conns under the idle timeout
		MaxIdleTimeout:  20 * time.Second, // detect dead clients/exits ~3x faster than the old 60s (dashboard accuracy)
	}
}

// DialDatagram dials a QUIC connection (datagrams enabled) over an arbitrary net.PacketConn — used
// for the Phase 2 direct path, where the PacketConn wraps the ICE-nominated path (see internal/ice).
// suppressBufWarn silences quic-go's one-time "connection doesn't allow setting of receive/send
// buffer size. Not a *net.UDPConn?" warning. The direct path runs QUIC over a pion ICE conn (a
// wrapped net.PacketConn), so quic-go cannot tune the socket buffer — the warning is unavoidable
// and purely a throughput note. We set the env var just-in-time, only for these direct conns, so
// the real-UDP relay sockets can still emit the useful "raise net.core.rmem_max" warning.
var suppressBufWarn sync.Once

func quietDatagramBufWarning() {
	suppressBufWarn.Do(func() {
		if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
			_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
		}
	})
}

func DialDatagram(ctx context.Context, pc net.PacketConn, remote net.Addr, tlsConf *tls.Config) (quic.Connection, error) {
	quietDatagramBufWarning()
	return quic.Dial(ctx, pc, remote, tlsConf, Config())
}

// ListenDatagram listens for a QUIC connection over a net.PacketConn (the controlled/exit side of
// the direct path).
func ListenDatagram(pc net.PacketConn, tlsConf *tls.Config) (*quic.Listener, error) {
	quietDatagramBufWarning()
	return quic.Listen(pc, tlsConf, Config())
}

// ServerTLS builds a self-signed TLS config for the broker/exit listeners (spike only).
func ServerTLS() (*tls.Config, error) {
	cert, err := selfSigned()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{proto.ALPN},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLS builds a client TLS config (spike: skips verification — DO NOT ship).
func ClientTLS() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // #nosec G402 -- spike only; replace with mTLS + token
		NextProtos:         []string{proto.ALPN},
		MinVersion:         tls.VersionTLS13,
	}
}

// ServerMTLS builds a server TLS config that REQUIRES and verifies a client certificate signed by
// the given CA (mutual TLS), presenting the given leaf. Use for the broker control listener.
func ServerMTLS(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errBadCA
	}
	return &tls.Config{
		Certificates: []tls.Certificate{leaf},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		NextProtos:   []string{proto.ALPN},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientMTLS builds a client TLS config that verifies the server against the given CA and presents
// the given leaf as the client certificate. serverName must match a SAN on the server's leaf.
func ClientMTLS(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errBadCA
	}
	return &tls.Config{
		Certificates: []tls.Certificate{leaf},
		RootCAs:      pool,
		ServerName:   serverName,
		NextProtos:   []string{proto.ALPN},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "revquic-spike"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:         true,
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
