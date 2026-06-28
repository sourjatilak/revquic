// SPDX-License-Identifier: GPL-3.0-or-later

package adminstore

import (
	"database/sql"
	"errors"
	"time"

	"github.com/sourjatilak/revquic/internal/pwhash"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

type sqlStore struct{ db *sql.DB }

const adminSchema = `
CREATE TABLE IF NOT EXISTS admins (
  username   TEXT PRIMARY KEY,
  role       TEXT NOT NULL,
  pass_hash  TEXT NOT NULL,
  created_at TEXT NOT NULL
);
`

// NewSQLite opens (creating if needed) a SQLite-backed admin store at path.
func NewSQLite(path string) (Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(adminSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqlStore{db: db}, nil
}

func (s *sqlStore) Create(username, password, role string) (Public, error) {
	if role == "" {
		role = RoleAdmin
	}
	hash, err := pwhash.Hash(password)
	if err != nil {
		return Public{}, err
	}
	_, err = s.db.Exec(`INSERT INTO admins(username,role,pass_hash,created_at) VALUES(?,?,?,?)`,
		username, role, hash, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Public{}, ErrConflict
	}
	return Public{Username: username, Role: role}, nil
}

func (s *sqlStore) Verify(username, password string) (Public, error) {
	var role, hash string
	err := s.db.QueryRow(`SELECT role,pass_hash FROM admins WHERE username=?`, username).Scan(&role, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Public{}, ErrBadCreds
	}
	if err != nil {
		return Public{}, err
	}
	if !pwhash.Verify(password, hash) {
		return Public{}, ErrBadCreds
	}
	return Public{Username: username, Role: role}, nil
}

func (s *sqlStore) List() []Public {
	rows, err := s.db.Query(`SELECT username,role FROM admins`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Public
	for rows.Next() {
		var p Public
		if err := rows.Scan(&p.Username, &p.Role); err == nil {
			out = append(out, p)
		}
	}
	return out
}
