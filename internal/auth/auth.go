// SPDX-License-Identifier: GPL-3.0-or-later

// Package auth provides the Phase 1 spike authentication primitives.
//
// Spike model (see spec/PHASE1.md): static tokens — an admin bearer token for the admin API, a
// shared node secret for exit (C) registration, and per-user client tokens held in userstore.
// Production replaces all of these with mTLS for nodes and OIDC/short-lived JWTs for clients.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// ConstantEqual compares two secrets in constant time.
func ConstantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// BearerToken extracts a bearer token from an Authorization header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// RequireAdmin wraps a handler with a constant-time admin bearer-token check.
func RequireAdmin(adminToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminToken == "" || !ConstantEqual(BearerToken(r), adminToken) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, `{"code":"unauthorized","message":"admin token required"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// RequireToken wraps a handler with a custom bearer-token validator (e.g. session lookup with a
// bootstrap-token fallback). The token may also be supplied via the access_token query parameter,
// a documented fallback for browser EventSource clients that cannot set Authorization headers.
func RequireToken(validate func(token string) bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := BearerToken(r)
		if tok == "" {
			tok = r.URL.Query().Get("access_token")
		}
		if tok == "" || !validate(tok) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, `{"code":"unauthorized","message":"valid token required"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
