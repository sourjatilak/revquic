// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux

// Package sysstat stub for non-Linux platforms. Host CPU/mem/disk sampling is implemented for Linux
// (where exit nodes run); elsewhere Sample() returns zeros.
package sysstat

// Stat holds utilization percentages (0..100).
type Stat struct {
	CPUPct  float64
	MemPct  float64
	DiskPct float64
}

// Sampler is a no-op on non-Linux platforms.
type Sampler struct{}

// Sample returns zero utilization on unsupported platforms.
func (s *Sampler) Sample() Stat { return Stat{} }
