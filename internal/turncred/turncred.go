// SPDX-License-Identifier: GPL-3.0-or-later

// Package turncred mints short-lived TURN credentials for the coturn REST API
// (draft-uberti-behave-turn-rest): the broker holds a shared static-auth-secret and issues per-session
// time-limited username/password pairs, so it never ships a long-term TURN secret to clients.
//
//	username = "<expiryUnix>:<sessionID>"
//	password = base64( HMAC-SHA1(secret, username) )
//
// coturn validates these with: turnserver --use-auth-secret --static-auth-secret=<secret>
package turncred

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

// Credentials returns a TURN username/password valid for ttl, bound to sessionID.
func Credentials(secret string, ttl time.Duration, sessionID string) (username, password string) {
	return CredentialsAt(secret, time.Now().Add(ttl), sessionID)
}

// CredentialsAt is Credentials with an explicit expiry (testable).
func CredentialsAt(secret string, expiry time.Time, sessionID string) (username, password string) {
	username = fmt.Sprintf("%d:%s", expiry.Unix(), sessionID)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	password = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return username, password
}

// Verify recomputes the password for username and compares in constant time (what coturn does).
func Verify(secret, username, password string) bool {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(password))
}
