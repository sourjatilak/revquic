// SPDX-License-Identifier: GPL-3.0-or-later

// Package pwhash hashes human passwords (admin accounts) with PBKDF2-HMAC-SHA256 using only the
// standard library. PBKDF2 (RFC 8018) is a standardized, FIPS-approved password KDF.
//
// Encoded form (django-style):  pbkdf2_sha256$<iterations>$<base64 salt>$<base64 derived-key>
//
// NOTE: argon2id is the preferred modern choice but lives in golang.org/x/crypto. Swap Hash/Verify
// to argon2id once that module is available; the encoded-string interface here stays the same.
// (This is for LOW-entropy human passwords. High-entropy bearer tokens use HMAC in userstore.)
package pwhash

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

const (
	algo       = "pbkdf2_sha256"
	iterations = 210000 // OWASP-recommended floor for PBKDF2-HMAC-SHA256 (2023)
	saltLen    = 16
	keyLen     = 32
)

// Hash returns an encoded PBKDF2 hash for the password (random per-call salt).
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2(password, salt, iterations, keyLen)
	return fmt.Sprintf("%s$%d$%s$%s", algo, iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// Verify reports whether password matches the encoded hash (constant-time).
func Verify(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != algo {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2(password, salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2 implements PBKDF2 (RFC 8018) over HMAC-SHA256.
func pbkdf2(password string, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, []byte(password))
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	dk := make([]byte, 0, numBlocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for x := range t {
				t[x] ^= u[x]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}
