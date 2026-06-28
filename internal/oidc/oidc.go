// SPDX-License-Identifier: GPL-3.0-or-later

// Package oidc verifies OpenID Connect ID tokens (RS256 JWTs) for Revquic user authentication. It
// validates the signature against the IdP's JWKS (provided statically or fetched from a jwks_uri),
// and checks issuer, audience, and expiry. Stdlib-only (no external JWT/OIDC dependency) so it is
// self-contained and offline-testable; for richer flows you can swap in coreos/go-oidc later.
package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Claims is the subset of ID-token claims Revquic uses.
type Claims struct {
	Subject  string
	Email    string
	Issuer   string
	Audience []string
	Expiry   int64
}

// Identity returns the preferred stable identity (email if present, else subject).
func (c *Claims) Identity() string {
	if c.Email != "" {
		return c.Email
	}
	return c.Subject
}

// Verifier validates ID tokens against a known issuer + audience using RSA public keys (JWKS).
type Verifier struct {
	issuer   string
	audience string

	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey // by kid
	jwksURL string
	hc      *http.Client
}

// NewStaticVerifier builds a verifier from an inline JWKS document.
func NewStaticVerifier(issuer, audience string, jwksJSON []byte) (*Verifier, error) {
	keys, err := parseJWKS(jwksJSON)
	if err != nil {
		return nil, err
	}
	return &Verifier{issuer: issuer, audience: audience, keys: keys}, nil
}

// NewURLVerifier builds a verifier that lazily fetches (and refreshes) the JWKS from jwksURL.
func NewURLVerifier(issuer, audience, jwksURL string) *Verifier {
	return &Verifier{issuer: issuer, audience: audience, jwksURL: jwksURL,
		keys: map[string]*rsa.PublicKey{}, hc: &http.Client{Timeout: 5 * time.Second}}
}

func b64urlDecode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func (v *Verifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	k := v.keys[kid]
	v.mu.RUnlock()
	if k != nil {
		return k, nil
	}
	if v.jwksURL == "" {
		return nil, fmt.Errorf("oidc: unknown key id %q", kid)
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	k = v.keys[kid]
	v.mu.RUnlock()
	if k == nil {
		return nil, fmt.Errorf("oidc: key id %q not in JWKS", kid)
	}
	return k, nil
}

func (v *Verifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: JWKS fetch status %d", resp.StatusCode)
	}
	var buf [1 << 20]byte
	n, _ := resp.Body.Read(buf[:])
	keys, err := parseJWKS(buf[:n])
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()
	return nil
}

// Verify validates the raw ID token and returns its claims. Checks: RS256 signature against the
// JWKS key (by kid), issuer match, audience contains our audience, and not expired.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("oidc: malformed token")
	}
	hdrBytes, err := b64urlDecode(parts[0])
	if err != nil {
		return nil, err
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, err
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("oidc: unsupported alg %q (want RS256)", hdr.Alg)
	}
	key, err := v.keyFor(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := b64urlDecode(parts[2])
	if err != nil {
		return nil, err
	}
	signingInput := parts[0] + "." + parts[1]
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return nil, fmt.Errorf("oidc: signature invalid: %w", err)
	}

	payload, err := b64urlDecode(parts[1])
	if err != nil {
		return nil, err
	}
	var claimsMap map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claimsMap); err != nil {
		return nil, err
	}
	c := &Claims{}
	_ = jsonStr(claimsMap["iss"], &c.Issuer)
	_ = jsonStr(claimsMap["sub"], &c.Subject)
	_ = jsonStr(claimsMap["email"], &c.Email)
	c.Audience = parseAud(claimsMap["aud"])
	c.Expiry = parseExp(claimsMap["exp"])

	if c.Issuer != v.issuer {
		return nil, fmt.Errorf("oidc: issuer %q != %q", c.Issuer, v.issuer)
	}
	if v.audience != "" && !contains(c.Audience, v.audience) {
		return nil, fmt.Errorf("oidc: audience %v missing %q", c.Audience, v.audience)
	}
	if c.Expiry == 0 || time.Now().Unix() >= c.Expiry {
		return nil, fmt.Errorf("oidc: token expired")
	}
	return c, nil
}

func jsonStr(r json.RawMessage, out *string) error {
	if len(r) == 0 {
		return nil
	}
	return json.Unmarshal(r, out)
}

func parseAud(r json.RawMessage) []string {
	if len(r) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return []string{s}
	}
	var a []string
	_ = json.Unmarshal(r, &a)
	return a
}

func parseExp(r json.RawMessage) int64 {
	if len(r) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(r, &f) == nil {
		return int64(f)
	}
	return 0
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// --- JWKS parsing (RSA) ---

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

func parseJWKS(data []byte) (map[string]*rsa.PublicKey, error) {
	var set jwks
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, err
	}
	out := make(map[string]*rsa.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := b64urlDecode(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := b64urlDecode(k.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eb { // big-endian exponent
			e = e<<8 | int(b)
		}
		if e == 0 {
			return nil, fmt.Errorf("oidc: bad RSA exponent for kid %q", k.Kid)
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("oidc: no RSA keys in JWKS")
	}
	return out, nil
}
