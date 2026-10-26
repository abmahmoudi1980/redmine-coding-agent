package domain_test

import (
	"errors"
	"testing"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

func TestTransitionTable(t *testing.T) {
	t.Parallel()

	allowed := map[domain.RunState][]domain.RunState{
		domain.StateQueued:       {domain.StatePreparing, domain.StateFailed, domain.StateCancelled},
		domain.StatePreparing:    {domain.StateImplementing, domain.StateFailed, domain.StateBlocked, domain.StateCancelled},
		domain.StateImplementing: {domain.StateValidating, domain.StateFailed, domain.StateBlocked, domain.StateCancelled},
		domain.StateValidating:   {domain.StateRepairing, domain.StatePublishing, domain.StateCompleted, domain.StateFailed, domain.StateCancelled},
		domain.StateRepairing:    {domain.StateValidating, domain.StateFailed, domain.StateCancelled},
		domain.StatePublishing:   {domain.StateReporting, domain.StateFailed, domain.StateCancelled},
		domain.StateReporting:    {domain.StateCompleted, domain.StateFailed},
	}

	for _, from := range domain.RunStates {
		for _, to := range domain.RunStates {
			wants := false
			for _, candidate := range allowed[from] {
				if candidate == to {
					wants = true
					break
				}
			}
			got := domain.CanTransition(from, to)
			if got != wants {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, wants)
			}
		}
	}
}

func TestNoTerminalStateExits(t *testing.T) {
	t.Parallel()

	terminals := []domain.RunState{
		domain.StateCompleted, domain.StateFailed, domain.StateBlocked,
		domain.StateCancelled, domain.StateInterrupted,
	}
	for _, from := range terminals {
		if !domain.IsTerminal(from) {
			t.Fatalf("%s should be terminal", from)
		}
		for _, to := range domain.RunStates {
			if domain.CanTransition(from, to) {
				t.Errorf("terminal %s must not transition to %s", from, to)
			}
		}
	}
}

func TestCheckTransitionConditions(t *testing.T) {
	t.Parallel()

	cond := domain.Conditions{CancelRequested: true, PublishingEnabled: false}
	if err := domain.CheckTransition(domain.StateImplementing, domain.StateCancelled, cond); err != nil {
		t.Fatalf("cancelled with a cancel request: %v", err)
	}
	if err := domain.CheckTransition(domain.StateImplementing, domain.StateCancelled, domain.Conditions{}); !errors.Is(err, domain.ErrCancelNotRequested) {
		t.Fatalf("cancelled without a request: got %v, want ErrCancelNotRequested", err)
	}
	if err := domain.CheckTransition(domain.StateValidating, domain.StateCompleted, domain.Conditions{}); err != nil {
		t.Fatalf("validating -> completed with publishing disabled: %v", err)
	}
	if err := domain.CheckTransition(domain.StateValidating, domain.StateCompleted, domain.Conditions{PublishingEnabled: true}); err == nil {
		t.Fatal("validating -> completed must be rejected while publishing is enabled")
	}
}

func TestInvalidTransitionCarriesBothStates(t *testing.T) {
	t.Parallel()

	err := domain.CheckTransition(domain.StateCompleted, domain.StatePreparing, domain.Conditions{})
	var transitionErr *domain.TransitionError
	if !errors.As(err, &transitionErr) {
		t.Fatalf("got %T (%v), want *TransitionError", err, err)
	}
	if transitionErr.From != domain.StateCompleted || transitionErr.To != domain.StatePreparing {
		t.Fatalf("carried %s -> %s", transitionErr.From, transitionErr.To)
	}
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("errors.Is(err, ErrInvalidTransition) = false")
	}
}

func TestInterruptedOnlyThroughRecovery(t *testing.T) {
	t.Parallel()

	for _, from := range domain.RunStates {
		if from == domain.StateInterrupted {
			continue
		}
		if domain.CanTransition(from, domain.StateInterrupted) {
			t.Errorf("CanTransition(%s, interrupted) must be false", from)
		}
		if err := domain.CheckTransition(from, domain.StateInterrupted, domain.Conditions{}); err == nil {
			t.Errorf("CheckTransition(%s, interrupted) must fail", from)
		}
	}

	for _, active := range domain.ActiveRunStates {
		if err := domain.MarkInterrupted(active); err != nil {
			t.Errorf("MarkInterrupted(%s): %v", active, err)
		}
	}
	for _, terminal := range []domain.RunState{
		domain.StateCompleted, domain.StateFailed, domain.StateBlocked,
		domain.StateCancelled, domain.StateInterrupted,
	} {
		if err := domain.MarkInterrupted(terminal); err == nil {
			t.Errorf("MarkInterrupted(%s) must fail", terminal)
		}
	}
}
