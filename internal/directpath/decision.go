// SPDX-License-Identifier: GPL-3.0-or-later

// Package directpath contains the broker's Phase 2 policy logic for the direct (P2P) path: the
// NAT-type-based decision of whether to attempt a direct A<->C hole punch or relay, and the
// relay<->direct session migration state machine. It is pure logic (no pion/ice or quic-go), so it
// is unit-testable offline. See spec/phase2-direct-path.md.
package directpath

// NATType classifies an endpoint's NAT, mirroring frp/pkg/nathole classification.
type NATType string

const (
	NATFullCone       NATType = "full-cone"
	NATRestrictedCone NATType = "restricted-cone"
	NATPortRestricted NATType = "port-restricted"
	NATSymmetric      NATType = "symmetric"
	NATUnknown        NATType = "unknown"
)

// Mode is the requested or decided data-path mode.
type Mode string

const (
	ModeAuto   Mode = "auto"   // requested: choose based on NAT types
	ModeDirect Mode = "direct" // requested/decided: P2P; if requested, no relay fallback
	ModeRelay  Mode = "relay"  // requested/decided: always via broker
)

// Decision is the outcome of the policy.
type Decision struct {
	// AttemptDirect is true if the session should run ICE to try a direct path.
	AttemptDirect bool
	// AllowFallback is true if a failed/lost direct path may fall back to relay.
	AllowFallback bool
	// Mode is the initial data-path mode (always ModeRelay when AttemptDirect, since we bootstrap
	// on the relay and upgrade; ModeRelay also when direct is not attempted).
	Mode Mode
	// Reason is a short human-readable explanation (for logs / admin UI).
	Reason string
}

// punchable reports whether a hole punch between two NAT types is worth attempting.
// Policy (see spec/phase2-direct-path.md §4):
//   - any pairing involving a full-cone side is punchable (cone side is predictable),
//   - cone/restricted/port-restricted pairings are punchable,
//   - symmetric paired with symmetric or restricted/port-restricted is NOT (port prediction unreliable),
//   - unknown is treated optimistically (attempt, then fall back).
func punchable(a, b NATType) bool {
	if a == NATFullCone || b == NATFullCone {
		return true
	}
	if a == NATSymmetric && b == NATSymmetric {
		return false
	}
	hard := func(t NATType) bool { return t == NATSymmetric }
	mediumOrHard := func(t NATType) bool {
		return t == NATSymmetric || t == NATRestrictedCone || t == NATPortRestricted
	}
	// symmetric on one side + (restricted/port-restricted/symmetric) on the other -> not punchable
	if (hard(a) && mediumOrHard(b)) || (hard(b) && mediumOrHard(a)) {
		return false
	}
	return true
}

// Decide returns the data-path decision for a session given the requested mode and the two NAT types.
func Decide(requested Mode, natA, natC NATType) Decision {
	switch requested {
	case ModeRelay:
		return Decision{AttemptDirect: false, AllowFallback: false, Mode: ModeRelay, Reason: "relay requested"}
	case ModeDirect:
		// Honor the request but do not silently relay; caller surfaces failure.
		return Decision{AttemptDirect: true, AllowFallback: false, Mode: ModeRelay, Reason: "direct requested (no fallback)"}
	default: // ModeAuto
		if natA == NATUnknown || natC == NATUnknown {
			return Decision{AttemptDirect: true, AllowFallback: true, Mode: ModeRelay, Reason: "unknown NAT: attempt direct, fall back to relay"}
		}
		if punchable(natA, natC) {
			return Decision{AttemptDirect: true, AllowFallback: true, Mode: ModeRelay, Reason: "NAT pair punchable: attempt direct"}
		}
		return Decision{AttemptDirect: false, AllowFallback: false, Mode: ModeRelay, Reason: "NAT pair not punchable (symmetric): relay"}
	}
}
