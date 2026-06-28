// SPDX-License-Identifier: GPL-3.0-or-later

package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"
)

// signToken builds and RS256-signs a JWT for tests.
func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdr := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	si := enc(hdr) + "." + enc(claims)
	sum := sha256.Sum256([]byte(si))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return si + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func jwksFor(key *rsa.PrivateKey, kid string) []byte {
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	eb := big.NewInt(int64(key.E)).Bytes()
	e := base64.RawURLEncoding.EncodeToString(eb)
	doc := map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": kid, "n": n, "e": e}}}
	b, _ := json.Marshal(doc)
	return b
}

func TestVerifyValidToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	v, err := NewStaticVerifier("https://idp.example.com", "revquic", jwksFor(key, "k1"))
	if err != nil {
		t.Fatal(err)
	}
	tok := signToken(t, key, "k1", map[string]any{
		"iss": "https://idp.example.com", "aud": "revquic",
		"sub": "user-123", "email": "alice@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	c, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.Identity() != "alice@example.com" {
		t.Errorf("identity=%q", c.Identity())
	}
}

func TestVerifyRejects(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	v, _ := NewStaticVerifier("https://idp.example.com", "revquic", jwksFor(key, "k1"))
	base := func() map[string]any {
		return map[string]any{"iss": "https://idp.example.com", "aud": "revquic", "sub": "u",
			"exp": time.Now().Add(time.Hour).Unix()}
	}

	cases := map[string]string{
		"wrong issuer":   signToken(t, key, "k1", merge(base(), map[string]any{"iss": "https://evil"})),
		"wrong audience": signToken(t, key, "k1", merge(base(), map[string]any{"aud": "other"})),
		"expired":        signToken(t, key, "k1", merge(base(), map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})),
		"unknown kid":    signToken(t, key, "k2", base()),
		"bad signature":  signToken(t, other, "k1", base()), // signed by a different key
	}
	for name, tok := range cases {
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

func TestMalformed(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	v, _ := NewStaticVerifier("iss", "aud", jwksFor(key, "k1"))
	for _, bad := range []string{"", "a.b", "a.b.c.d", fmt.Sprintf("%s..", "x")} {
		if _, err := v.Verify(context.Background(), bad); err == nil {
			t.Errorf("malformed %q: expected error", bad)
		}
	}
}
