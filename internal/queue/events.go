package queue

// Subscribe returns a channel of completion events and a function to stop
// receiving them.
//
// This exists mostly for tests, and it is the single biggest testing win in
// the package. Without it, an integration test has to sleep for "long enough"
// after enqueuing and hope the job ran — which is how queue test suites become
// the flaky ones everybody skips. With it, a test subscribes, enqueues, and
// waits on the event with a deadline.
//
// Delivery is non-blocking: a subscriber that stops reading is skipped rather
// than allowed to stall the worker that is trying to report. A test that
// misses an event fails; a production pool that blocked on one would stop.
func (p *Pool) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 16
	}

	ch := make(chan Event, buffer)

	p.subMu.Lock()
	p.subID++
	id := p.subID
	p.subs[id] = ch
	p.subMu.Unlock()

	return ch, func() {
		p.subMu.Lock()
		defer p.subMu.Unlock()
		if sub, ok := p.subs[id]; ok {
			delete(p.subs, id)
			close(sub)
		}
	}
}

// publish reports a finished job to every subscriber.
func (p *Pool) publish(e Event) {
	p.subMu.Lock()
	defer p.subMu.Unlock()

	for _, ch := range p.subs {
		select {
		case ch <- e:
		default:
			// Dropped rather than blocked. The worker's job is to run jobs,
			// not to wait for an observer.
		}
	}
}
