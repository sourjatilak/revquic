// SPDX-License-Identifier: GPL-3.0-or-later

// Package telemetry sends periodic per-session QoS reports (MsgReport) from an endpoint
// (client or exit) to the broker, so the broker can track throughput, drops, and the direct
// path it cannot otherwise observe. Endpoints provide a serialized control writer.
package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/sourjatilak/revquic/internal/proto"
	"github.com/sourjatilak/revquic/internal/session"
)

// DefaultInterval is the report cadence when none is given.
const DefaultInterval = 5 * time.Second

// CtrlWriter is a serialized control-stream writer (so reports don't interleave with other
// writers on the same stream). Satisfied by the client/exit control writers.
type CtrlWriter interface {
	WriteControl(*proto.Control) error
}

// Meta is the static, per-endpoint context included in every report (host, TUN interface, and the
// STUN/TURN status of the direct path). PathKind ("relay"/"direct") is derived per-tick from sess.
// Downlink marks this endpoint as the egress/exit side, so its byte counter is reported as download
// (BytesDown) rather than upload — this keeps up/down accurate even on the broker-bypassing direct
// path, where the broker can't meter traffic itself.
type Meta struct {
	Host      string
	TunName   string
	OS        string
	UsingStun bool
	UsingTurn bool
	Downlink  bool
	// HostStat, when set, supplies the reporting host's CPU/mem/disk utilization (0..100) included
	// in each report. The client wires this to a periodically-sampled sysstat reading; the exit
	// leaves it nil (it reports host stats separately via MsgNodeStatus).
	HostStat func() (cpu, mem, disk float64)
}

// RTT holds the most recently measured round-trip latency (milliseconds), updated by the endpoint's
// control reader when it sees a MsgPong, and read by Run when composing the next report.
type RTT struct {
	ms atomic.Int64
}

// Observe records a round-trip measured from a pong echoing tsNanos.
func (r *RTT) Observe(tsNanos int64) {
	if r == nil || tsNanos == 0 {
		return
	}
	r.ms.Store((time.Now().UnixNano() - tsNanos) / 1e6)
}

// Millis returns the last measured RTT in milliseconds (0 if unknown).
func (r *RTT) Millis() int {
	if r == nil {
		return 0
	}
	return int(r.ms.Load())
}

// throughput computes bytes/sec between two cumulative byte counts over dt seconds.
func throughput(cur, prev uint64, dt float64) float64 {
	if dt <= 0 || cur < prev {
		return 0
	}
	return float64(cur-prev) / dt
}

// Run reports sess's stats to the broker every interval until ctx is cancelled. Each tick it also
// sends a MsgPing (the broker echoes MsgPong, which the caller's control reader feeds back via rtt)
// so the report can carry latency. meta supplies static context; rtt may be nil to skip latency.
func Run(ctx context.Context, w CtrlWriter, sid uint64, interval time.Duration, sess *session.Session, meta Meta, rtt *RTT) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastBytes uint64
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// Latency probe for the next report.
			_ = w.WriteControl(&proto.Control{Type: proto.MsgPing, SessionID: sid, TS: now.UnixNano()})

			b, _, d := sess.Stats()
			bps := throughput(b, lastBytes, now.Sub(last).Seconds())
			lastBytes, last = b, now
			pathKind := "relay"
			if sess.IsDirect() {
				pathKind = "direct"
			}
			rep := &proto.Control{
				Type: proto.MsgReport, SessionID: sid,
				ThroughputBps: bps, Drops: d, Direct: sess.IsDirect(),
				RTTms: rtt.Millis(),
				Host:  meta.Host, TunName: meta.TunName, PathKind: pathKind,
				UsingStun: meta.UsingStun, UsingTurn: meta.UsingTurn, OS: meta.OS,
			}
			if meta.Downlink {
				rep.BytesDown = b
			} else {
				rep.BytesUp = b
			}
			if meta.HostStat != nil {
				rep.CPUPct, rep.MemPct, rep.DiskPct = meta.HostStat()
			}
			_ = w.WriteControl(rep)
		}
	}
}
