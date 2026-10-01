package controller

import "sync"

// events is a tiny publish/subscribe hub. The web UI subscribes (via SSE)
// and refreshes the matching view when a topic such as "nodes" changes.
// Publishing never blocks: a slow subscriber just misses intermediate
// notifications, which is fine because each one means "refetch".
type events struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newEvents() *events { return &events{subs: map[chan string]struct{}{}} }

func (e *events) publish(topic string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- topic:
		default:
		}
	}
}

// Subscribe returns a channel of topic names and a function to unsubscribe.
func (c *Controller) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 16)
	c.events.mu.Lock()
	c.events.subs[ch] = struct{}{}
	c.events.mu.Unlock()
	return ch, func() {
		c.events.mu.Lock()
		delete(c.events.subs, ch)
		c.events.mu.Unlock()
	}
}
