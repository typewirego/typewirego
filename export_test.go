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
