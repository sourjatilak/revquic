// SPDX-License-Identifier: GPL-3.0-or-later

package directpath

import "testing"

func TestDecide(t *testing.T) {
	cases := []struct {
		name        string
		mode        Mode
		a, c        NATType
		wantAttempt bool
		wantFallbck bool
	}{
		{"relay forced", ModeRelay, NATFullCone, NATFullCone, false, false},
		{"direct forced no fallback", ModeDirect, NATSymmetric, NATSymmetric, true, false},
		{"auto cone+cone", ModeAuto, NATFullCone, NATPortRestricted, true, true},
		{"auto restricted+restricted", ModeAuto, NATRestrictedCone, NATPortRestricted, true, true},
		{"auto fullcone+symmetric", ModeAuto, NATFullCone, NATSymmetric, true, true},
		{"auto symmetric+restricted -> relay", ModeAuto, NATSymmetric, NATRestrictedCone, false, false},
		{"auto symmetric+symmetric -> relay", ModeAuto, NATSymmetric, NATSymmetric, false, false},
		{"auto unknown -> attempt+fallback", ModeAuto, NATUnknown, NATSymmetric, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.mode, tc.a, tc.c)
			if d.AttemptDirect != tc.wantAttempt {
				t.Errorf("AttemptDirect=%v want %v (%s)", d.AttemptDirect, tc.wantAttempt, d.Reason)
			}
			if d.AllowFallback != tc.wantFallbck {
				t.Errorf("AllowFallback=%v want %v (%s)", d.AllowFallback, tc.wantFallbck, d.Reason)
			}
			if d.Mode != ModeRelay {
				t.Errorf("Mode=%v want relay bootstrap (%s)", d.Mode, d.Reason)
			}
		})
	}
}

func TestPunchableSymmetry(t *testing.T) {
	types := []NATType{NATFullCone, NATRestrictedCone, NATPortRestricted, NATSymmetric}
	for _, a := range types {
		for _, b := range types {
			if punchable(a, b) != punchable(b, a) {
				t.Errorf("punchable not symmetric for (%s,%s)", a, b)
			}
		}
	}
}

func TestStateMachineHappyPath(t *testing.T) {
	m := NewMachine()
	steps := []struct {
		ev   Event
		want PathState
	}{
		{EvStartRelay, StateRelaying},
		{EvBeginChecks, StateChecking},
		{EvDirectEstablished, StateDirect},
		{EvDirectLost, StateRelaying}, // fall back
		{EvBeginChecks, StateChecking},
		{EvChecksFailed, StateRelaying}, // stay relayed
		{EvClose, StateClosed},
	}
	for i, s := range steps {
		got, err := m.Fire(s.ev)
		if err != nil {
			t.Fatalf("step %d %s: %v", i, s.ev, err)
		}
		if got != s.want {
			t.Fatalf("step %d %s: state=%s want %s", i, s.ev, got, s.want)
		}
	}
	if !m.IsTerminal() {
		t.Error("expected terminal state")
	}
}

func TestStateMachineRejectsInvalid(t *testing.T) {
	m := NewMachine()
	// cannot begin checks before relay bootstrap
	if _, err := m.Fire(EvBeginChecks); err == nil {
		t.Error("expected invalid transition New->BeginChecks")
	}
	// cannot go relay->direct without checking
	_, _ = m.Fire(EvStartRelay)
	if _, err := m.Fire(EvDirectEstablished); err == nil {
		t.Error("expected invalid transition Relaying->DirectEstablished")
	}
	// after close, nothing fires
	_, _ = m.Fire(EvClose)
	if _, err := m.Fire(EvStartRelay); err == nil {
		t.Error("expected no transitions out of Closed")
	}
}
