// SPDX-License-Identifier: GPL-3.0-or-later

package telemetry

import "testing"

func TestThroughput(t *testing.T) {
	cases := []struct {
		cur, prev uint64
		dt        float64
		want      float64
	}{
		{cur: 1000, prev: 0, dt: 1, want: 1000},
		{cur: 5000, prev: 1000, dt: 2, want: 2000},
		{cur: 100, prev: 0, dt: 0, want: 0},  // no elapsed time
		{cur: 50, prev: 100, dt: 1, want: 0}, // counter reset / wrap guard
		{cur: 0, prev: 0, dt: 5, want: 0},    // idle
	}
	for i, c := range cases {
		if got := throughput(c.cur, c.prev, c.dt); got != c.want {
			t.Errorf("case %d: throughput(%d,%d,%g)=%g want %g", i, c.cur, c.prev, c.dt, got, c.want)
		}
	}
}
