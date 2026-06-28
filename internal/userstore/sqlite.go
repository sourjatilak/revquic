// SPDX-License-Identifier: GPL-3.0-or-later

package userstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sourjatilak/revquic/internal/adminapi"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (cgo-free)
)

// sqlStore is a SQLite-backed Store. Credentials are stored as HMAC-SHA256(pepper, token); region
// policy is a JSON array. Implements the same Store interface as the in-memory/file backends.
type sqlStore struct {
	db     *sql.DB
	pepper string
}

const userSchema = `
CREATE TABLE IF NOT EXISTS users (
  id              TEXT PRIMARY KEY,
  username        TEXT UNIQUE NOT NULL,
  token_hash      TEXT,
  allowed_regions TEXT NOT NULL,
  status          TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_token_hash ON users(token_hash);
`

// NewSQLite opens (creating if needed) a SQLite-backed user store at path.
func NewSQLite(path, pepper string) (Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writers
	if _, err := db.Exec(userSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqlStore{db: db, pepper: pepper}, nil
}

func marshalRegions(r []string) string { b, _ := json.Marshal(r); return string(b) }
func unmarshalRegions(s string) []string {
	var r []string
	_ = json.Unmarshal([]byte(s), &r)
	return r
}

func (s *sqlStore) Create(in adminapi.UserCreate) (adminapi.User, error) {
	status := in.Status
	if status == "" {
		status = adminapi.UserEnabled
	}
	now := time.Now().UTC()
	u := adminapi.User{
		ID: newID(), Username: in.Username, AllowedRegions: in.AllowedRegions,
		Status: status, CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.Exec(
		`INSERT INTO users(id,username,token_hash,allowed_regions,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		u.ID, u.Username, hmacToken(s.pepper, in.Credential), marshalRegions(u.AllowedRegions), u.Status,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		// UNIQUE(username) violation -> conflict
		return adminapi.User{}, ErrConflict
	}
	return u, nil
}

func scanUser(row interface{ Scan(...any) error }) (adminapi.User, error) {
	var u adminapi.User
	var regions, created, updated string
	if err := row.Scan(&u.ID, &u.Username, &regions, &u.Status, &created, &updated); err != nil {
		return adminapi.User{}, err
	}
	u.AllowedRegions = unmarshalRegions(regions)
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	u.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return u, nil
}

func (s *sqlStore) Get(id string) (adminapi.User, error) {
	row := s.db.QueryRow(`SELECT id,username,allowed_regions,status,created_at,updated_at FROM users WHERE id=?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return adminapi.User{}, ErrNotFound
	}
	return u, err
}

func (s *sqlStore) List(region, status string) []adminapi.User {
	rows, err := s.db.Query(`SELECT id,username,allowed_regions,status,created_at,updated_at FROM users`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []adminapi.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			continue
		}
		if status != "" && u.Status != status {
			continue
		}
		if region != "" && !regionAllowed(u.AllowedRegions, region) {
			continue
		}
		out = append(out, u)
	}
	return out
}

func (s *sqlStore) Update(id string, in adminapi.UserUpdate) (adminapi.User, error) {
	u, err := s.Get(id)
	if err != nil {
		return adminapi.User{}, err
	}
	if in.AllowedRegions != nil {
		u.AllowedRegions = *in.AllowedRegions
	}
	if in.Status != nil {
		u.Status = *in.Status
	}
	u.UpdatedAt = time.Now().UTC()
	if in.Credential != nil {
		_, err = s.db.Exec(`UPDATE users SET allowed_regions=?,status=?,updated_at=?,token_hash=? WHERE id=?`,
			marshalRegions(u.AllowedRegions), u.Status, u.UpdatedAt.Format(time.RFC3339Nano), hmacToken(s.pepper, *in.Credential), id)
	} else {
		_, err = s.db.Exec(`UPDATE users SET allowed_regions=?,status=?,updated_at=? WHERE id=?`,
			marshalRegions(u.AllowedRegions), u.Status, u.UpdatedAt.Format(time.RFC3339Nano), id)
	}
	return u, err
}

func (s *sqlStore) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) AuthenticateForRegion(token, region string) (adminapi.User, error) {
	if token == "" {
		return adminapi.User{}, ErrBadToken
	}
	row := s.db.QueryRow(`SELECT id,username,allowed_regions,status,created_at,updated_at FROM users WHERE token_hash=?`, hmacToken(s.pepper, token))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return adminapi.User{}, ErrBadToken
	}
	if err != nil {
		return adminapi.User{}, err
	}
	if u.Status != adminapi.UserEnabled {
		return adminapi.User{}, ErrDisabled
	}
	if !regionAllowed(u.AllowedRegions, region) {
		return adminapi.User{}, ErrRegionDenied
	}
	return u, nil
}

func (s *sqlStore) AuthorizeUser(username, region string) (adminapi.User, error) {
	row := s.db.QueryRow(`SELECT id,username,allowed_regions,status,created_at,updated_at FROM users WHERE username=?`, username)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return adminapi.User{}, ErrNotFound
	}
	if err != nil {
		return adminapi.User{}, err
	}
	if u.Status != adminapi.UserEnabled {
		return adminapi.User{}, ErrDisabled
	}
	if !regionAllowed(u.AllowedRegions, region) {
		return adminapi.User{}, ErrRegionDenied
	}
	return u, nil
}
