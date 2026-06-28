// SPDX-License-Identifier: GPL-3.0-or-later

package userstore

import (
	"path/filepath"
	"testing"

	"github.com/sourjatilak/revquic/internal/adminapi"
)

func TestSQLiteStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.db")
	s, err := NewSQLite(path, "pepper")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	u, err := s.Create(adminapi.UserCreate{Username: "alice", Credential: "tok-a", AllowedRegions: []string{"us-west"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Create(adminapi.UserCreate{Username: "alice", Credential: "x"}); err != ErrConflict {
		t.Errorf("dup username: want ErrConflict, got %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-a", "us-west"); err != nil {
		t.Errorf("auth allowed: %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-a", "eu-central"); err != ErrRegionDenied {
		t.Errorf("auth region: want ErrRegionDenied, got %v", err)
	}
	if _, err := s.AuthenticateForRegion("wrong", "us-west"); err != ErrBadToken {
		t.Errorf("bad token: want ErrBadToken, got %v", err)
	}
	if _, err := s.AuthorizeUser("alice", "us-west"); err != nil {
		t.Errorf("authorize: %v", err)
	}

	// credential rotation
	nc := "tok-a2"
	if _, err := s.Update(u.ID, adminapi.UserUpdate{Credential: &nc}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-a", "us-west"); err != ErrBadToken {
		t.Errorf("old token after rotation: want ErrBadToken, got %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-a2", "us-west"); err != nil {
		t.Errorf("new token: %v", err)
	}

	// reopen -> persisted; no plaintext token on disk
	s2, err := NewSQLite(path, "pepper")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(s2.List("", "")) != 1 {
		t.Fatalf("want 1 persisted user, got %d", len(s2.List("", "")))
	}
	if _, err := s2.AuthenticateForRegion("tok-a2", "us-west"); err != nil {
		t.Errorf("persisted token after reopen: %v", err)
	}
	// wrong pepper rejects
	s3, _ := NewSQLite(path, "other")
	if _, err := s3.AuthenticateForRegion("tok-a2", "us-west"); err != ErrBadToken {
		t.Errorf("wrong pepper: want ErrBadToken, got %v", err)
	}
}
