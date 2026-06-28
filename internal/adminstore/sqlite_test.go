// SPDX-License-Identifier: GPL-3.0-or-later

package adminstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteAdminStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admins.db")
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.Create("root", "hunter2", RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Create("root", "x", RoleAdmin); err != ErrConflict {
		t.Errorf("dup: want ErrConflict, got %v", err)
	}
	if p, err := s.Verify("root", "hunter2"); err != nil || p.Role != RoleAdmin {
		t.Errorf("verify: %+v %v", p, err)
	}
	if _, err := s.Verify("root", "wrong"); err != ErrBadCreds {
		t.Errorf("bad pass: want ErrBadCreds, got %v", err)
	}

	// no plaintext password on disk
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("plaintext password leaked to disk")
	}

	// reopen -> persisted
	s2, _ := NewSQLite(path)
	if _, err := s2.Verify("root", "hunter2"); err != nil {
		t.Errorf("persisted verify after reopen: %v", err)
	}
	if len(s2.List()) != 1 {
		t.Fatalf("want 1 admin, got %d", len(s2.List()))
	}
}
