// SPDX-License-Identifier: GPL-3.0-or-later

package adminstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateVerify(t *testing.T) {
	s := New()
	if _, err := s.Create("admin", "s3cret", RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Create("admin", "x", RoleAdmin); err != ErrConflict {
		t.Errorf("duplicate: want ErrConflict, got %v", err)
	}
	if p, err := s.Verify("admin", "s3cret"); err != nil || p.Role != RoleAdmin {
		t.Errorf("verify good: %+v %v", p, err)
	}
	if _, err := s.Verify("admin", "wrong"); err != ErrBadCreds {
		t.Errorf("verify bad pass: want ErrBadCreds, got %v", err)
	}
	if _, err := s.Verify("nobody", "s3cret"); err != ErrBadCreds {
		t.Errorf("verify unknown user: want ErrBadCreds, got %v", err)
	}
}

func TestFilePersistenceNoPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admins.json")
	s1, err := NewFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s1.Create("root", "hunter2", RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("plaintext admin password leaked to disk")
	}
	s2, err := NewFile(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.Verify("root", "hunter2"); err != nil {
		t.Errorf("persisted admin should verify after reopen: %v", err)
	}
}
