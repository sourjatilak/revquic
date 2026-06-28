// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

// Package sysstat samples host CPU / memory / disk utilization on Linux from /proc and statfs.
// It is dependency-free. Use a single Sampler and call Sample() periodically — CPU% is computed
// from the delta between successive calls (the first call primes the baseline and returns 0% CPU).
package sysstat

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Stat holds utilization percentages (0..100).
type Stat struct {
	CPUPct  float64
	MemPct  float64
	DiskPct float64
}

// Sampler retains the previous CPU reading so successive Sample() calls can compute a delta.
type Sampler struct {
	lastIdle  uint64
	lastTotal uint64
}

// Sample returns current CPU/mem/disk utilization. CPU% is the busy fraction since the previous
// call; the first call returns CPU=0 (no baseline yet).
func (s *Sampler) Sample() Stat {
	var st Stat
	if idle, total, ok := readCPU(); ok {
		if s.lastTotal != 0 && total > s.lastTotal {
			dTotal := float64(total - s.lastTotal)
			dIdle := float64(idle - s.lastIdle)
			if dTotal > 0 {
				st.CPUPct = clamp((1 - dIdle/dTotal) * 100)
			}
		}
		s.lastIdle, s.lastTotal = idle, total
	}
	st.MemPct = readMem()
	st.DiskPct = readDisk("/")
	return st
}

func readCPU() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var idleAll uint64
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idleAll += v
		}
	}
	return idleAll, total, true
}

func readMem() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	var memTotal, memAvail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			memTotal = v
		case "MemAvailable:":
			memAvail = v
		}
	}
	if memTotal == 0 {
		return 0
	}
	return clamp((1 - float64(memAvail)/float64(memTotal)) * 100)
}

func readDisk(path string) float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return 0
	}
	return clamp((1 - float64(free)/float64(total)) * 100)
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
