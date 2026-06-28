// SPDX-License-Identifier: GPL-3.0-or-later

package userstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourjatilak/revquic/internal/adminapi"
)

func TestAuthenticateForRegion(t *testing.T) {
	s := New("pepper")
	u, err := s.Create(adminapi.UserCreate{Username: "alice", Credential: "tok-a", AllowedRegions: []string{"us-west"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.AuthenticateForRegion("tok-a", "us-west"); err != nil {
		t.Errorf("allowed region should pass: %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-a", "eu-central"); err != ErrRegionDenied {
		t.Errorf("disallowed region: want ErrRegionDenied, got %v", err)
	}
	if _, err := s.AuthenticateForRegion("wrong", "us-west"); err != ErrBadToken {
		t.Errorf("bad token: want ErrBadToken, got %v", err)
	}
	if _, err := s.AuthenticateForRegion("", "us-west"); err != ErrBadToken {
		t.Errorf("empty token: want ErrBadToken, got %v", err)
	}

	if _, err := s.Update(u.ID, adminapi.UserUpdate{AllowedRegions: &[]string{"*"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-a", "anywhere"); err != nil {
		t.Errorf("wildcard should allow any region: %v", err)
	}

	disabled := adminapi.UserDisabled
	if _, err := s.Update(u.ID, adminapi.UserUpdate{Status: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-a", "anywhere"); err != ErrDisabled {
		t.Errorf("disabled user: want ErrDisabled, got %v", err)
	}
}

func TestAuthorizeUser(t *testing.T) {
	s := New("pepper")
	u, _ := s.Create(adminapi.UserCreate{Username: "alice@example.com", AllowedRegions: []string{"us-west"}})
	if _, err := s.AuthorizeUser("alice@example.com", "us-west"); err != nil {
		t.Errorf("allowed: %v", err)
	}
	if _, err := s.AuthorizeUser("alice@example.com", "eu-central"); err != ErrRegionDenied {
		t.Errorf("disallowed region: want ErrRegionDenied, got %v", err)
	}
	if _, err := s.AuthorizeUser("nobody@example.com", "us-west"); err != ErrNotFound {
		t.Errorf("unknown user: want ErrNotFound, got %v", err)
	}
	disabled := adminapi.UserDisabled
	_, _ = s.Update(u.ID, adminapi.UserUpdate{Status: &disabled})
	if _, err := s.AuthorizeUser("alice@example.com", "us-west"); err != ErrDisabled {
		t.Errorf("disabled: want ErrDisabled, got %v", err)
	}
}

func TestCreateConflictAndDelete(t *testing.T) {
	s := New("pepper")
	if _, err := s.Create(adminapi.UserCreate{Username: "bob", Credential: "tok-b", AllowedRegions: []string{"*"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Create(adminapi.UserCreate{Username: "bob", Credential: "tok-b2"}); err != ErrConflict {
		t.Errorf("duplicate username: want ErrConflict, got %v", err)
	}
	users := s.List("", "")
	if len(users) != 1 {
		t.Fatalf("want 1 user, got %d", len(users))
	}
	if err := s.Delete(users[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.AuthenticateForRegion("tok-b", "x"); err != ErrBadToken {
		t.Errorf("after delete token should be invalid, got %v", err)
	}
}

func TestCredentialUpdateRehashes(t *testing.T) {
	s := New("pepper")
	u, _ := s.Create(adminapi.UserCreate{Username: "carol", Credential: "old", AllowedRegions: []string{"*"}})
	newCred := "new"
	if _, err := s.Update(u.ID, adminapi.UserUpdate{Credential: &newCred}); err != nil {
		t.Fatalf("update cred: %v", err)
	}
	if _, err := s.AuthenticateForRegion("old", "x"); err != ErrBadToken {
		t.Errorf("old token should be invalid after rotation, got %v", err)
	}
	if _, err := s.AuthenticateForRegion("new", "x"); err != nil {
		t.Errorf("new token should authenticate: %v", err)
	}
}

func TestFilePersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")

	s1, err := NewFile(path, "pepper")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s1.Create(adminapi.UserCreate{Username: "dave", Credential: "tok-d", AllowedRegions: []string{"us-west"}}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The plaintext token must NOT appear on disk.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(raw), "tok-d") {
		t.Fatal("plaintext credential leaked to disk")
	}

	// Reopen: the user and its hashed credential must survive.
	s2, err := NewFile(path, "pepper")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(s2.List("", "")) != 1 {
		t.Fatalf("want 1 persisted user, got %d", len(s2.List("", "")))
	}
	if _, err := s2.AuthenticateForRegion("tok-d", "us-west"); err != nil {
		t.Errorf("persisted token should authenticate after reopen: %v", err)
	}

	// A different pepper must NOT validate the same token (hash is keyed).
	s3, err := NewFile(path, "other-pepper")
	if err != nil {
		t.Fatalf("reopen w/ other pepper: %v", err)
	}
	if _, err := s3.AuthenticateForRegion("tok-d", "us-west"); err != ErrBadToken {
		t.Errorf("wrong pepper: want ErrBadToken, got %v", err)
	}
}
