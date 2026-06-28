// SPDX-License-Identifier: GPL-3.0-or-later

package conf

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyFileBasicsAndPrecedence(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	broker := fs.String("broker", "default:1", "")
	region := fs.String("region", "", "")
	direct := fs.Bool("direct", false, "")
	rate := fs.Float64("rate", 0, "")
	// region is provided on the "CLI" and must win over the file.
	if err := fs.Parse([]string{"-region", "cli-region"}); err != nil {
		t.Fatal(err)
	}
	cfg := writeTemp(t, `
# comment
; also comment
broker = file-broker:4242
region = file-region
direct
rate = "12.5"
`)
	if err := ApplyFile(fs, cfg); err != nil {
		t.Fatalf("ApplyFile: %v", err)
	}
	if *broker != "file-broker:4242" {
		t.Errorf("broker=%q", *broker)
	}
	if *region != "cli-region" {
		t.Errorf("region=%q (CLI should win)", *region)
	}
	if !*direct {
		t.Errorf("direct should be true from bare key")
	}
	if *rate != 12.5 {
		t.Errorf("rate=%v", *rate)
	}
}

func TestApplyFileUnknownKey(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("broker", "", "")
	_ = fs.Parse(nil)
	cfg := writeTemp(t, "nope = 1\n")
	if err := ApplyFile(fs, cfg); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestApplyFileBadValue(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Int("n", 0, "")
	_ = fs.Parse(nil)
	cfg := writeTemp(t, "n = notanint\n")
	if err := ApplyFile(fs, cfg); err == nil {
		t.Fatal("expected error for invalid int value")
	}
}
