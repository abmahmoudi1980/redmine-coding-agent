package domain

import (
	"errors"
	"fmt"
)

// ErrInvalidTransition is the sentinel for a rejected state change.
var ErrInvalidTransition = errors.New("invalid run state transition")

// ErrCancelNotRequested is returned when a transition to cancelled is tried
// without a recorded cancel request (FR-013).
var ErrCancelNotRequested = errors.New("cancelled requires a cancel request")

// TransitionError carries both ends of a rejected transition.
type TransitionError struct {
	From RunState
	To   RunState
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid run state transition: %s -> %s", e.From, e.To)
}

func (e *TransitionError) Is(target error) bool {
	return target == ErrInvalidTransition
}

var transitions = map[RunState][]RunState{
	StateQueued:       {StatePreparing, StateFailed, StateCancelled},
	StatePreparing:    {StateImplementing, StateFailed, StateBlocked, StateCancelled},
	StateImplementing: {StateValidating, StateFailed, StateBlocked, StateCancelled},
	StateValidating:   {StateRepairing, StatePublishing, StateCompleted, StateFailed, StateCancelled},
	StateRepairing:    {StateValidating, StateFailed, StateCancelled},
	StatePublishing:   {StateReporting, StateFailed, StateCancelled},
	StateReporting:    {StateCompleted, StateFailed},
}

// CanTransition reports whether the state machine permits from -> to.
// interrupted is deliberately excluded: it is only ever written by the
// startup recovery sweep through MarkInterrupted.
func CanTransition(from, to RunState) bool {
	if to == StateInterrupted {
		return false
	}
	for _, candidate := range transitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no transition may leave s.
func IsTerminal(state RunState) bool {
	switch state {
	case StateCompleted, StateFailed, StateBlocked, StateCancelled, StateInterrupted:
		return true
	}
	return false
}

// Conditions are the runtime facts a transition depends on beyond the table.
type Conditions struct {
	// CancelRequested must be true before a run may enter cancelled.
	CancelRequested bool
	// PublishingEnabled distinguishes the "nothing to publish" path
	// validating -> completed from an attempt to skip publishing.
	PublishingEnabled bool
}

// CheckTransition validates from -> to against both the transition table and
// the runtime conditions that make a transition meaningful.
func CheckTransition(from, to RunState, cond Conditions) error {
	if to == StateInterrupted {
		return fmt.Errorf("%w: interrupted is reserved for the startup recovery sweep", ErrInvalidTransition)
	}
	if !from.Valid() || !to.Valid() {
		return &TransitionError{From: from, To: to}
	}
	if !CanTransition(from, to) {
		return &TransitionError{From: from, To: to}
	}
	if to == StateCancelled && !cond.CancelRequested {
		return fmt.Errorf("%w: %s -> %s", ErrCancelNotRequested, from, to)
	}
	if from == StateValidating && to == StateCompleted && cond.PublishingEnabled {
		return fmt.Errorf("%w: validating -> completed requires publishing to be disabled", ErrInvalidTransition)
	}
	return nil
}

// MarkInterrupted applies the recovery sweep's only legal write: any
// non-terminal run becomes interrupted, and nothing else may.
func MarkInterrupted(from RunState) error {
	if !from.Valid() {
		return &TransitionError{From: from, To: StateInterrupted}
	}
	if IsTerminal(from) {
		return fmt.Errorf("%w: %s -> interrupted", ErrInvalidTransition, from)
	}
	return nil
}
