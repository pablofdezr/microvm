package sandbox

import "time"

// WithRetention forgets a stopped sandbox once d has elapsed since it stopped.
//
// A stopped sandbox is kept deliberately -- it is how a caller who polls learns
// why theirs died, and where the final metering is served from -- but nothing
// used to drop it afterwards, so a long-lived daemon's map and its list pages
// grew for the life of the process.
//
// Zero is the default and forgets nothing, which is what every node did before
// this existed. Reaping records an operator never asked to reap would silently
// change what their node answers about last hour's sandboxes.
//
// The window is raised to the log store's when it is shorter; see
// retentionFloor.
func WithRetention(d time.Duration) Option {
	return func(m *Manager) { m.retention = d }
}

// retentionFloor is how long stopped sandboxes are really kept: never less than
// their output.
//
// Derived rather than configured, because the two windows are not independent
// and two flags an operator sets by hand can be put in the wrong order. A
// stopped record holds the sandbox's final metering -- sampled just before the
// kill, unrecoverable after it, and the only statement of what the run cost --
// and every exec record is reached through its sandbox, so forgetting the
// sandbox first would delete a bill nobody has read and 404 output the host is
// still holding.
//
// Zero stays zero: no window means nothing is forgotten, and a floor must not
// switch a reaper on for a node that asked for none.
func retentionFloor(want, logRetention time.Duration) time.Duration {
	if want <= 0 {
		return 0
	}
	return max(want, logRetention)
}

// Retention is the window stopped sandboxes are kept for, after the floor, or
// zero when they are kept forever.
//
// It is the effective value rather than what was asked for, which is the point:
// the daemon logs it and paces its sweep by it, and both would be wrong if they
// read a number that had been raised.
func (m *Manager) Retention() time.Duration { return m.retention }

// Sweep forgets stopped sandboxes whose retention has elapsed, returning how
// many went. Without it the map grows for the daemon's whole life.
func (m *Manager) Sweep() int {
	if m.retention <= 0 {
		return 0
	}

	cutoff := time.Now().Add(-m.retention)

	m.mu.Lock()
	defer m.mu.Unlock()

	var dropped int
	for id, sb := range m.sandboxes {
		// The manager's lock, then the sandbox's. That is the one order this
		// package takes them in -- stop() releases the sandbox's before calling
		// release() -- and taking them in two orders is what would deadlock a
		// sweep against a sandbox stopping underneath it.
		sb.mu.Lock()
		state, stoppedAt := sb.state, sb.stoppedAt
		sb.mu.Unlock()

		// Never forget a running sandbox, however old: it is a live VM and this map
		// is the only handle on it, so dropping the entry would leave nothing able
		// to stop it and nobody able to ask about it.
		if state != StateStopped || stoppedAt.After(cutoff) {
			continue
		}

		// The sandbox's exec records go with it, because it is the only handle on
		// them: past this delete they are reachable through no route. Their own
		// window has already elapsed -- retentionFloor keeps this one at or above
		// it -- and a log store with no window of its own drops nothing, so without
		// this a node that reaps sandboxes leaks their output for the daemon's whole
		// life. The manager's lock, then the store's; the store never takes this
		// one, so there is no order to invert.
		m.logs.ForgetSandbox(id)

		delete(m.sandboxes, id)
		dropped++
	}
	return dropped
}
