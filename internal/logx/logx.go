// SPDX-License-Identifier: GPL-3.0-or-later

// Package logx configures the standard library logger for the binaries: an optional log file (else
// stderr) and a log type of "text" (default, timestamped lines) or "json" (one JSON object per
// line). It wraps the existing log.Printf call sites — no code changes needed at the call sites.
package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

// Setup points the standard logger at the given file (or stderr if empty) using the given format
// ("text" or "json"). It returns a close function (close the file on shutdown). Note: writes are
// unbuffered, so logs are durable even if the process exits via os.Exit without calling it.
func Setup(component, file, format string) (func() error, error) {
	out := io.Writer(os.Stderr)
	closeFn := func() error { return nil }
	if file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open log file %q: %w", file, err)
		}
		out = f
		closeFn = f.Close
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		log.SetFlags(log.LstdFlags)
		log.SetOutput(out)
	case "json":
		log.SetFlags(0)
		log.SetOutput(&jsonWriter{out: out, component: component})
	default:
		_ = closeFn()
		return nil, fmt.Errorf("unknown log type %q (want text|json)", format)
	}
	return closeFn, nil
}

// jsonWriter wraps each std-log line (one Write call per log message) into a JSON object. The log
// package serializes Output with a mutex, so writes here are never interleaved.
type jsonWriter struct {
	out       io.Writer
	component string
}

func (w *jsonWriter) Write(p []byte) (int, error) {
	rec := map[string]string{
		"time":      time.Now().UTC().Format(time.RFC3339Nano),
		"component": w.component,
		"msg":       strings.TrimRight(string(p), "\n"),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	if _, err := w.out.Write(append(b, '\n')); err != nil {
		return 0, err
	}
	return len(p), nil
}
