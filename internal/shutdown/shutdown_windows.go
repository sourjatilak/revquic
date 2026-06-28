// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package shutdown

import (
	"os"
	"syscall"
)

// interactiveSignals: Windows has no SIGTSTP (Ctrl-Z), so only Ctrl-C and SIGTERM.
var interactiveSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

const keyHint = "Ctrl-C"

func keyName(s os.Signal) string {
	if s == syscall.SIGINT {
		return "Ctrl-C"
	}
	return s.String()
}
