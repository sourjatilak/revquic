// SPDX-License-Identifier: GPL-3.0-or-later

package logx

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupJSONToFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.json")
	closeFn, err := Setup("exit", p, "json")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	log.Printf("hello %d", 7)
	_ = closeFn()
	// restore default logger for other tests
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("not valid JSON line: %q (%v)", b, err)
	}
	if rec["component"] != "exit" || rec["msg"] != "hello 7" || rec["time"] == "" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestSetupTextToFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log.txt")
	closeFn, err := Setup("broker", p, "text")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	log.Print("plain-line")
	_ = closeFn()
	log.SetOutput(os.Stderr)

	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "plain-line") {
		t.Errorf("text log missing message: %q", b)
	}
}

func TestSetupBadType(t *testing.T) {
	if _, err := Setup("x", "", "xml"); err == nil {
		t.Fatal("expected error for unknown log type")
	}
	log.SetOutput(os.Stderr)
}
