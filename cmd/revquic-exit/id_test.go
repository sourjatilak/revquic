// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"My Home Exit":   "my-home-exit",
		"  Spaced  Out ": "spaced-out",
		"Café #1!!":      "caf-1",
		"UPPER_case-42":  "upper-case-42",
		"___":            "",
		"":               "",
		"a.b.c":          "a-b-c",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveNodeID(t *testing.T) {
	// Explicit id always wins (trimmed).
	if got := deriveNodeID("  exit-7 ", "Some Name"); got != "exit-7" {
		t.Errorf("explicit id: got %q want exit-7", got)
	}
	// Name-derived: slug + "-" + 4-digit suffix.
	got := deriveNodeID("", "My Home Exit")
	if !strings.HasPrefix(got, "my-home-exit-") {
		t.Fatalf("name-derived id %q lacks slug prefix", got)
	}
	if suf := strings.TrimPrefix(got, "my-home-exit-"); len(suf) != 4 {
		t.Errorf("name-derived suffix %q is not 4 digits", suf)
	}
	// Neither: random exit-<n>.
	rnd := deriveNodeID("", "")
	if !strings.HasPrefix(rnd, "exit-") || len(strings.TrimPrefix(rnd, "exit-")) != 4 {
		t.Errorf("random id %q not of form exit-<4 digits>", rnd)
	}
	// A name that slugifies to empty falls back to exit-<n>.
	if g := deriveNodeID("", "###"); !strings.HasPrefix(g, "exit-") {
		t.Errorf("empty-slug name should fall back to exit-, got %q", g)
	}
}
