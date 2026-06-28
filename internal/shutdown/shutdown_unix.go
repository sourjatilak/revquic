// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package shutdown

import (
	"os"
	"syscall"
)

// interactiveSignals: Ctrl-C, SIGTERM, and Ctrl-Z (job-control suspend, which we intercept).
var interactiveSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGTSTP}

const keyHint = "Ctrl-C / Ctrl-Z"

func keyName(s os.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "Ctrl-C"
	case syscall.SIGTSTP:
		return "Ctrl-Z"
	default:
		return s.String()
	}
}
