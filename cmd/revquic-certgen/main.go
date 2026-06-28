// SPDX-License-Identifier: GPL-3.0-or-later

// Command revquic-certgen generates a self-hosted CA and the broker/node/client leaf certificates
// used for Revquic control-plane mTLS. Writes PEM files to -out. Spike convenience; for production use
// your own CA / SPIFFE and rotate regularly.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sourjatilak/revquic/internal/pki"
)

func main() {
	out := flag.String("out", "certs", "output directory")
	brokerSANs := flag.String("broker-san", "server,localhost,broker", "comma-separated DNS SANs for the broker (must include the host clients dial)")
	validDays := flag.Int("days", 365, "validity in days")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	valid := time.Duration(*validDays) * 24 * time.Hour

	caCert, caKey, err := pki.GenerateCA("Revquic CA", valid)
	if err != nil {
		log.Fatalf("ca: %v", err)
	}
	write(*out, "ca.pem", caCert, 0o644)
	write(*out, "ca-key.pem", caKey, 0o600)

	// broker server cert (server auth) with SANs.
	dns := splitCSV(*brokerSANs)
	brokerCert, brokerKey, err := pki.IssueLeaf(caCert, caKey, "broker", dns, []net.IP{net.ParseIP("127.0.0.1")}, true, false, valid)
	if err != nil {
		log.Fatalf("broker leaf: %v", err)
	}
	write(*out, "broker-cert.pem", brokerCert, 0o644)
	write(*out, "broker-key.pem", brokerKey, 0o600)

	// node (exit) cert: server + client auth (exit is a client to the broker AND the TLS server on
	// the direct P2P path). client cert: client auth only.
	specs := []struct {
		who            string
		server, client bool
	}{
		{"node", true, true},
		{"client", false, true},
	}
	for _, s := range specs {
		c, k, err := pki.IssueLeaf(caCert, caKey, s.who, nil, nil, s.server, s.client, valid)
		if err != nil {
			log.Fatalf("%s leaf: %v", s.who, err)
		}
		write(*out, s.who+"-cert.pem", c, 0o644)
		write(*out, s.who+"-key.pem", k, 0o600)
	}
	log.Printf("wrote CA + broker/node/client certs to %s (broker SANs: %s)", *out, *brokerSANs)
}

func write(dir, name string, data []byte, mode os.FileMode) {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, mode); err != nil {
		log.Fatalf("write %s: %v", p, err)
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
