package api

import (
	"sync"

	"github.com/systemgroup/redmine-coding-agent/internal/domain"
)

// defaultSubscriberBuffer is how many timeline events a slow client may fall
// behind before the oldest are dropped. The durable log in run_events is the
// real record; the hub exists to keep one browser stream current.
const defaultSubscriberBuffer = 128

// Subscription is one live listener's view of a run's timeline.
type Subscription struct {
	// C yields events with increasing seq. The channel is never closed, so a
	// reader must also watch its request context.
	C <-chan domain.RunEvent
}

// Hub fans persisted run events out to the streams watching that run. It is
// deliberately lossy for slow clients and never blocks the runner.
type Hub struct {
	mu     sync.Mutex
	buffer int
	runs   map[int64]map[*subscriber]struct{}
}

type subscriber struct {
	events chan domain.RunEvent
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{
		buffer: defaultSubscriberBuffer,
		runs:   make(map[int64]map[*subscriber]struct{}),
	}
}

// Subscribe registers one listener for a run. The returned cancel function is
// idempotent and must be called when the stream closes.
func (h *Hub) Subscribe(runID int64) (*Subscription, func()) {
	if h == nil {
		return &Subscription{C: make(chan domain.RunEvent)}, func() {}
	}
	h.mu.Lock()
	buffer := h.buffer
	if buffer <= 0 {
		buffer = defaultSubscriberBuffer
	}
	listener := &subscriber{events: make(chan domain.RunEvent, buffer)}
	if h.runs == nil {
		h.runs = make(map[int64]map[*subscriber]struct{})
	}
	subscribers, ok := h.runs[runID]
	if !ok {
		subscribers = make(map[*subscriber]struct{})
		h.runs[runID] = subscribers
	}
	subscribers[listener] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if current, ok := h.runs[runID]; ok {
				delete(current, listener)
				if len(current) == 0 {
					delete(h.runs, runID)
				}
			}
		})
	}
	return &Subscription{C: listener.events}, cancel
}

// Publish delivers one committed event. A full buffer loses its oldest entry
// rather than stalling the writer (SC-009: cancellation can never wait on a
// browser).
func (h *Hub) Publish(runID int64, event domain.RunEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for listener := range h.runs[runID] {
		select {
		case listener.events <- event:
		default:
			select {
			case <-listener.events:
			default:
			}
			select {
			case listener.events <- event:
			default:
			}
		}
	}
}

// Subscribers reports how many streams watch a run, for tests and diagnostics.
func (h *Hub) Subscribers(runID int64) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.runs[runID])
}
