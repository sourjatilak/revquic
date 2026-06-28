// SPDX-License-Identifier: GPL-3.0-or-later

// Package adminstore holds administrator accounts for the broker management plane. Unlike VPN
// users (high-entropy bearer tokens, hashed with HMAC in userstore), admins authenticate with
// human passwords, hashed via PBKDF2 (internal/pwhash). Accounts persist to a JSON file so they
// survive restarts. Same Store-interface pattern as userstore for backend swappability.
package adminstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sourjatilak/revquic/internal/pwhash"
)

// Roles.
const (
	RoleAdmin    = "admin"
	RoleReadOnly = "readonly"
)

var (
	ErrNotFound = errors.New("admin not found")
	ErrConflict = errors.New("admin already exists")
	ErrBadCreds = errors.New("invalid credentials")
)

// Admin is an administrator account. The password hash is never exported in API responses.
type Admin struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	PassHash  string    `json:"passHash"`
	CreatedAt time.Time `json:"createdAt"`
}

// Public is the admin view without the password hash.
type Public struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Store is the admin account registry interface.
type Store interface {
	Create(username, password, role string) (Public, error)
	Verify(username, password string) (Public, error)
	List() []Public
}

type table struct {
	mu   sync.RWMutex
	byU  map[string]*Admin
	path string
}

// New returns an in-memory admin store.
func New() Store { return &table{byU: map[string]*Admin{}} }

// NewFile returns a file-persisted admin store, loading existing accounts at path.
func NewFile(path string) (Store, error) {
	t := &table{byU: map[string]*Admin{}, path: path}
	if err := t.load(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *table) Create(username, password, role string) (Public, error) {
	if role == "" {
		role = RoleAdmin
	}
	hash, err := pwhash.Hash(password)
	if err != nil {
		return Public{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byU[username]; ok {
		return Public{}, ErrConflict
	}
	a := &Admin{Username: username, Role: role, PassHash: hash, CreatedAt: time.Now().UTC()}
	t.byU[username] = a
	if err := t.flushLocked(); err != nil {
		return Public{}, err
	}
	return Public{Username: a.Username, Role: a.Role}, nil
}

func (t *table) Verify(username, password string) (Public, error) {
	t.mu.RLock()
	a, ok := t.byU[username]
	t.mu.RUnlock()
	if !ok || !pwhash.Verify(password, a.PassHash) {
		return Public{}, ErrBadCreds
	}
	return Public{Username: a.Username, Role: a.Role}, nil
}

func (t *table) List() []Public {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Public, 0, len(t.byU))
	for _, a := range t.byU {
		out = append(out, Public{Username: a.Username, Role: a.Role})
	}
	return out
}

func (t *table) load() error {
	if t.path == "" {
		return nil
	}
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var admins []Admin
	if err := json.Unmarshal(b, &admins); err != nil {
		return err
	}
	for i := range admins {
		a := admins[i]
		t.byU[a.Username] = &a
	}
	return nil
}

func (t *table) flushLocked() error {
	if t.path == "" {
		return nil
	}
	admins := make([]Admin, 0, len(t.byU))
	for _, a := range t.byU {
		admins = append(admins, *a)
	}
	b, err := json.MarshalIndent(admins, "", "  ")
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
