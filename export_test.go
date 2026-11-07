package typewirego

import (
	"context"
	"time"
)

// Test-only hooks. They live in package typewirego so they are not public API.

// NodeIDOf returns the graph node id of a bound token.
func NodeIDOf(c *Container, t Token) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.slots[t]
	if s == nil {
		return "", false
	}
	return s.id, true
}

// EventQueueCap is the observer's queue capacity.
const EventQueueCap = eventQueueCap

// CleanupGrace is the grace a bounded pass waits past its bound.
const CleanupGrace = cleanupGrace

// ErrClassOf is the class Event.ErrClass reports for err.
func ErrClassOf(err error) string { return errClass(err) }

// FlushBudgetFor is the Flush allowance Teardown fixes at now for ctx.
func FlushBudgetFor(ctx context.Context, configured time.Duration, now time.Time) time.Duration {
	return flushBudgetFor(ctx, configured, now)
}

// ObserverWaiting reports whether the observer's resolver is waiting on t.
func ObserverWaiting(c *Container, t Token) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.obs != nil && c.obs.waiting[t] > 0
}

// SetBetweenEventsHook installs f to run on the drainer between deliveries.
func SetBetweenEventsHook(c *Container, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.obs.betweenEvents = f
}
