package agent

import "sync"

// eventBroker only wakes stream readers. SQLite remains the source of truth,
// so a slow or disconnected client can always resume from its last cursor.
type eventBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
}

func newEventBroker() *eventBroker {
	return &eventBroker{subscribers: map[string]map[chan struct{}]struct{}{}}
}

func (b *eventBroker) subscribe(turnID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subscribers[turnID] == nil {
		b.subscribers[turnID] = map[chan struct{}]struct{}{}
	}
	b.subscribers[turnID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subscribers[turnID], ch)
		if len(b.subscribers[turnID]) == 0 {
			delete(b.subscribers, turnID)
		}
		b.mu.Unlock()
	}
}

func (b *eventBroker) notify(turnID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers[turnID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
