// SPDX-License-Identifier: GPL-3.0-or-later

package directpath

import (
	"fmt"
	"sync"
)

// PathState is a session's data-path state (see spec/phase2-direct-path.md §5).
type PathState string

const (
	StateNew      PathState = "new"
	StateRelaying PathState = "relaying"
	StateChecking PathState = "checking"
	StateDirect   PathState = "direct"
	StateClosed   PathState = "closed"
)

// Event drives the state machine.
type Event string

const (
	EvStartRelay        Event = "start_relay"        // bootstrap; traffic begins on the relay
	EvBeginChecks       Event = "begin_checks"       // start ICE connectivity checks
	EvDirectEstablished Event = "direct_established" // a pair was nominated; migrate pumps
	EvChecksFailed      Event = "checks_failed"      // ICE failed/timed out; stay relayed
	EvDirectLost        Event = "direct_lost"        // direct path died; fall back to relay
	EvClose             Event = "close"              // terminal
)

// ErrInvalidTransition is returned for a disallowed (state, event) pair.
type ErrInvalidTransition struct {
	From  PathState
	Event Event
}

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("directpath: invalid transition from %q on %q", e.From, e.Event)
}

// transitions defines the allowed state graph. Anything not listed is rejected.
var transitions = map[PathState]map[Event]PathState{
	StateNew: {
		EvStartRelay: StateRelaying,
		EvClose:      StateClosed,
	},
	StateRelaying: {
		EvBeginChecks: StateChecking,
		EvClose:       StateClosed,
	},
	StateChecking: {
		EvDirectEstablished: StateDirect,
		EvChecksFailed:      StateRelaying,
		EvClose:             StateClosed,
	},
	StateDirect: {
		EvDirectLost: StateRelaying, // relay is always the safety net
		EvClose:      StateClosed,
	},
}

// Machine is a concurrency-safe data-path state machine for one session.
type Machine struct {
	mu    sync.Mutex
	state PathState
}

// NewMachine returns a machine in StateNew.
func NewMachine() *Machine { return &Machine{state: StateNew} }

// State returns the current state.
func (m *Machine) State() PathState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Fire applies an event, returning the new state or an ErrInvalidTransition.
func (m *Machine) Fire(ev Event) (PathState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, ok := transitions[m.state][ev]
	if !ok {
		return m.state, ErrInvalidTransition{From: m.state, Event: ev}
	}
	m.state = next
	return next, nil
}

// IsTerminal reports whether the session is closed.
func (m *Machine) IsTerminal() bool { return m.State() == StateClosed }
