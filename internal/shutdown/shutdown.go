// SPDX-License-Identifier: GPL-3.0-or-later

// Package shutdown provides a small interactive-exit helper shared by the client and exit. SIGTERM
// (e.g. `docker stop`, systemd) triggers an immediate clean shutdown so orchestration is never
// blocked. Ctrl-C (SIGINT) and, on Unix, Ctrl-Z (SIGTSTP) require TWO presses within a short window
// — the first prints a hint, the second exits — so an accidental keystroke doesn't tear down the
// tunnel. The per-platform signal set lives in shutdown_unix.go / shutdown_windows.go.
package shutdown

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// OnSignals installs the signal handler. onExit must perform the clean shutdown (e.g. close the
// broker connection) and terminate the process; it is invoked at most once. logf is used for the
// "press again" hint (pass log.Printf).
func OnSignals(window time.Duration, logf func(string, ...any), onExit func()) {
	if window <= 0 {
		window = 3 * time.Second
	}
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, interactiveSignals...)
	go func() {
		var last time.Time
		for s := range sigc {
			if s == syscall.SIGTERM {
				onExit()
				return
			}
			// SIGINT (Ctrl-C) or SIGTSTP (Ctrl-Z): require a confirming second press.
			now := time.Now()
			if !last.IsZero() && now.Sub(last) <= window {
				onExit()
				return
			}
			last = now
			logf("received %s — press %s again within %s to exit", keyName(s), keyHint, window)
		}
	}()
}
