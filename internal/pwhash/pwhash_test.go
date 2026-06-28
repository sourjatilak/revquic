// SPDX-License-Identifier: GPL-3.0-or-later

package pwhash

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, algo+"$") {
		t.Fatalf("unexpected encoding: %s", h)
	}
	if !Verify("correct horse battery staple", h) {
		t.Error("correct password should verify")
	}
	if Verify("wrong password", h) {
		t.Error("wrong password must not verify")
	}
}

func TestHashIsSaltedUnique(t *testing.T) {
	a, _ := Hash("same")
	b, _ := Hash("same")
	if a == b {
		t.Error("two hashes of the same password must differ (random salt)")
	}
	if !Verify("same", a) || !Verify("same", b) {
		t.Error("both salted hashes must verify")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "x", "pbkdf2_sha256$abc$zz$zz", "other$1$YQ$Yg", "pbkdf2_sha256$1$!$!"} {
		if Verify("p", bad) {
			t.Errorf("malformed encoding must not verify: %q", bad)
		}
	}
}

// Known-answer vector for PBKDF2-HMAC-SHA256 (RFC 7914 §11): P="passwd", S="salt",
// c=1, dkLen=64. First 64 bytes must match the published vector.
func TestPBKDF2KnownAnswer(t *testing.T) {
	got := pbkdf2("passwd", []byte("salt"), 1, 64)
	want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc" +
		"49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"
	if hex.EncodeToString(got) != want {
		t.Errorf("PBKDF2 KAT mismatch:\n got=%s\nwant=%s", hex.EncodeToString(got), want)
	}
}
