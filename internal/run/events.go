package run

import (
	"sync"
	"time"
)

// Event is one progress notification for the live-run view.
type Event struct {
	RunID   int64     `json:"run_id"`
	Type    string    `json:"type"`
	Payload any       `json:"payload"`
	At      time.Time `json:"at"`
}

// Broker fans events out to SSE subscribers. Slow subscribers drop events
// rather than blocking the run.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]int64 // channel -> run filter (0 = all runs)
}

func NewBroker() *Broker { return &Broker{subs: map[chan Event]int64{}} }

func (b *Broker) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, filter := range b.subs {
		if filter != 0 && filter != e.RunID {
			continue
		}
		select {
		case ch <- e:
		default: // drop for slow consumers
		}
	}
}

// Subscribe returns a channel of events for one run (or all runs when
// runID is 0) and an unsubscribe func.
func (b *Broker) Subscribe(runID int64) (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subs[ch] = runID
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}
}
