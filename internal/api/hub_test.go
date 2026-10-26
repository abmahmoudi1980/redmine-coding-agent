package api

import (
	"testing"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

func TestHubCloseEndsEverySubscription(t *testing.T) {
	hub := NewHub()
	subscription, unsubscribe := hub.Subscribe(7)
	defer unsubscribe()

	hub.Publish(7, domain.RunEvent{Seq: 1, Type: domain.EventStage})
	select {
	case event := <-subscription.C:
		if event.Seq != 1 {
			t.Fatalf("seq = %d", event.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("published event did not arrive")
	}

	hub.Close()
	select {
	case _, ok := <-subscription.C:
		if ok {
			t.Fatal("a closed hub must close its subscriber channels")
		}
	case <-time.After(time.Second):
		t.Fatal("hub.Close did not end the subscription")
	}
	if hub.Subscribers(7) != 0 {
		t.Fatal("hub still reports subscribers after Close")
	}
	// Publishing after shutdown must be harmless, not a send on a closed channel.
	hub.Publish(7, domain.RunEvent{Seq: 2, Type: domain.EventStage})
}
