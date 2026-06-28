// SPDX-License-Identifier: GPL-3.0-or-later

// Package conf loads a simple key=value config file into a flag.FlagSet. Command-line flags always
// take precedence: only flags NOT already set on the command line are filled from the file, so you
// can keep everything in a config file and still override individual values on the CLI.
//
// File format (one setting per line):
//
//	# comments are full-line only (start the line with # or ;)
//	broker = broker.example.com:4242
//	region = us-west
//	direct = true
//	token  = "quoted values are unquoted; everything after = is taken verbatim"
//
// The "=" is optional ("broker broker.example.com:4242" also works), and a bare key on its own line
// sets a boolean flag to true. Keys are flag names, with or without a leading "-".
package conf

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

type boolFlag interface{ IsBoolFlag() bool }

// ApplyFile reads path and applies each setting to fs, skipping any flag already provided on the
// command line. It returns an error for unknown keys or invalid values so typos fail loudly.
func ApplyFile(fs *flag.FlagSet, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	setOnCLI := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { setOnCLI[fl.Name] = true })

	sc := bufio.NewScanner(f)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		rawKey, rawVal := splitKV(line)
		key := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rawKey), "-"))
		if key == "" {
			return fmt.Errorf("%s:%d: missing key", path, ln)
		}
		fl := fs.Lookup(key)
		if fl == nil {
			return fmt.Errorf("%s:%d: unknown config key %q", path, ln, key)
		}
		if setOnCLI[key] {
			continue // command line wins
		}
		val := unquote(strings.TrimSpace(rawVal))
		if val == "" {
			if bf, ok := fl.Value.(boolFlag); ok && bf.IsBoolFlag() {
				val = "true" // bare boolean key
			}
		}
		if err := fs.Set(key, val); err != nil {
			return fmt.Errorf("%s:%d: set %q: %w", path, ln, key, err)
		}
	}
	return sc.Err()
}

func splitKV(line string) (string, string) {
	if i := strings.IndexByte(line, '='); i >= 0 {
		return line[:i], line[i+1:]
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], line[i+1:]
	}
	return line, ""
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
