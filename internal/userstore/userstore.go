// SPDX-License-Identifier: GPL-3.0-or-later

// Package userstore is the VPN user registry: persisted user records, per-user region policy,
// and bearer-token authentication. It enforces the region policy the admin UI configures.
//
// Credential handling: a user's credential is a HIGH-ENTROPY bearer token (API-key-like), not a
// human password. It is stored as HMAC-SHA256(pepper, token) — a fast keyed hash that (a) never
// stores the plaintext token and (b) still allows O(1) lookup on auth. (argon2/bcrypt are for
// low-entropy passwords and cannot be looked up by token because of per-record salts.)
//
// Backends: New() is in-memory; NewFile() persists to a JSON file (atomic writes) so the broker
// survives restarts. Both implement Store. A SQLite/Postgres backend (production) implements the
// same interface — see spec/PHASE1.md for the schema.
package userstore

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sourjatilak/revquic/internal/adminapi"
)

var (
	ErrNotFound     = errors.New("user not found")
	ErrConflict     = errors.New("username already exists")
	ErrBadToken     = errors.New("invalid token")
	ErrDisabled     = errors.New("user disabled")
	ErrRegionDenied = errors.New("region not allowed for user")
)

// Store is the backend-agnostic user registry interface. Swap implementations (in-memory,
// file, SQLite, Postgres) without changing callers.
type Store interface {
	Create(in adminapi.UserCreate) (adminapi.User, error)
	Get(id string) (adminapi.User, error)
	List(region, status string) []adminapi.User
	Update(id string, in adminapi.UserUpdate) (adminapi.User, error)
	Delete(id string) error
	AuthenticateForRegion(token, region string) (adminapi.User, error)
	// AuthorizeUser checks an already-authenticated identity (e.g. an OIDC subject/email mapped to a
	// username) against status + region policy, without a bearer token.
	AuthorizeUser(username, region string) (adminapi.User, error)
}

// record is the on-disk/in-memory unit: the public user plus its hashed token.
type record struct {
	User adminapi.User `json:"user"`
	Hash string        `json:"hash"` // HMAC-SHA256(pepper, token); "" if no credential
}

// table is the shared implementation. path == "" means in-memory only.
type table struct {
	mu     sync.RWMutex
	byID   map[string]*record
	byHash map[string]*record
	pepper string
	path   string
}

// New returns an in-memory store.
func New(pepper string) Store {
	return &table{byID: map[string]*record{}, byHash: map[string]*record{}, pepper: pepper}
}

// NewFile returns a file-persisted store, loading any existing records at path.
func NewFile(path, pepper string) (Store, error) {
	t := &table{byID: map[string]*record{}, byHash: map[string]*record{}, pepper: pepper, path: path}
	if err := t.load(); err != nil {
		return nil, err
	}
	return t, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// hashToken keys the token with the server pepper. Empty token -> empty hash.
func (t *table) hashToken(token string) string {
	return hmacToken(t.pepper, token)
}

// hmacToken returns HMAC-SHA256(pepper, token) hex; empty token -> empty hash.
func hmacToken(pepper, token string) string {
	if token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

func (t *table) Create(in adminapi.UserCreate) (adminapi.User, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.byID {
		if r.User.Username == in.Username {
			return adminapi.User{}, ErrConflict
		}
	}
	status := in.Status
	if status == "" {
		status = adminapi.UserEnabled
	}
	now := time.Now().UTC()
	r := &record{
		User: adminapi.User{
			ID:             newID(),
			Username:       in.Username,
			AllowedRegions: in.AllowedRegions,
			Status:         status,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		Hash: t.hashToken(in.Credential),
	}
	t.byID[r.User.ID] = r
	if r.Hash != "" {
		t.byHash[r.Hash] = r
	}
	if err := t.flushLocked(); err != nil {
		return adminapi.User{}, err
	}
	return r.User, nil
}

func (t *table) Get(id string) (adminapi.User, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.byID[id]
	if !ok {
		return adminapi.User{}, ErrNotFound
	}
	return r.User, nil
}

func (t *table) List(region, status string) []adminapi.User {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]adminapi.User, 0, len(t.byID))
	for _, r := range t.byID {
		if status != "" && r.User.Status != status {
			continue
		}
		if region != "" && !regionAllowed(r.User.AllowedRegions, region) {
			continue
		}
		out = append(out, r.User)
	}
	return out
}

func (t *table) Update(id string, in adminapi.UserUpdate) (adminapi.User, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.byID[id]
	if !ok {
		return adminapi.User{}, ErrNotFound
	}
	if in.AllowedRegions != nil {
		r.User.AllowedRegions = *in.AllowedRegions
	}
	if in.Status != nil {
		r.User.Status = *in.Status
	}
	if in.Credential != nil {
		delete(t.byHash, r.Hash)
		r.Hash = t.hashToken(*in.Credential)
		if r.Hash != "" {
			t.byHash[r.Hash] = r
		}
	}
	r.User.UpdatedAt = time.Now().UTC()
	if err := t.flushLocked(); err != nil {
		return adminapi.User{}, err
	}
	return r.User, nil
}

func (t *table) Delete(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.byID[id]
	if !ok {
		return ErrNotFound
	}
	delete(t.byID, id)
	delete(t.byHash, r.Hash)
	return t.flushLocked()
}

func (t *table) AuthenticateForRegion(token, region string) (adminapi.User, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.byHash[t.hashToken(token)]
	if !ok || token == "" {
		return adminapi.User{}, ErrBadToken
	}
	if r.User.Status != adminapi.UserEnabled {
		return adminapi.User{}, ErrDisabled
	}
	if !regionAllowed(r.User.AllowedRegions, region) {
		return adminapi.User{}, ErrRegionDenied
	}
	return r.User, nil
}

// AuthorizeUser checks an already-authenticated identity (username) against status + region policy.
func (t *table) AuthorizeUser(username, region string) (adminapi.User, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, r := range t.byID {
		if r.User.Username == username {
			if r.User.Status != adminapi.UserEnabled {
				return adminapi.User{}, ErrDisabled
			}
			if !regionAllowed(r.User.AllowedRegions, region) {
				return adminapi.User{}, ErrRegionDenied
			}
			return r.User, nil
		}
	}
	return adminapi.User{}, ErrNotFound
}

func regionAllowed(allowed []string, region string) bool {
	for _, a := range allowed {
		if a == "*" || a == region {
			return true
		}
	}
	return false
}

// --- persistence (file backend) ---

func (t *table) load() error {
	if t.path == "" {
		return nil
	}
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // fresh store
	}
	if err != nil {
		return err
	}
	var recs []record
	if err := json.Unmarshal(b, &recs); err != nil {
		return err
	}
	for i := range recs {
		r := recs[i]
		t.byID[r.User.ID] = &r
		if r.Hash != "" {
			t.byHash[r.Hash] = &r
		}
	}
	return nil
}

// flushLocked persists all records atomically. Caller must hold t.mu. No-op when in-memory.
func (t *table) flushLocked() error {
	if t.path == "" {
		return nil
	}
	recs := make([]record, 0, len(t.byID))
	for _, r := range t.byID {
		recs = append(recs, *r)
	}
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(t.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}
