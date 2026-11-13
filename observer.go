package typewirego

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Observer receives a container's lifecycle events. OnEvent is called from
// a single goroutine, one event at a time, outside every container lock,
// and never while Flush runs. Flush runs at most once, during Teardown,
// after the last event delivered; it is skipped when the drainer is abandoned
// inside OnEvent, and a Flush that overruns its allowance may still be running
// after Teardown returns (Wait waits for it).
type Observer interface {
	OnEvent(e Event)
	Flush(ctx context.Context) error
}

// ObserverFactory builds the observer from the only container cutout it gets.
// It runs inside NewContainer, where every resolve through r fails.
type ObserverFactory func(r Resolver) Observer

// EventKind names a lifecycle event.
type EventKind string

// The lifecycle events, in the order a wire meets them.
const (
	EventRegistered    EventKind = "registered"
	EventResolveStart  EventKind = "resolve-start"
	EventResolveOK     EventKind = "resolve-ok"
	EventResolveFailed EventKind = "resolve-failed"
	EventCycleDetected EventKind = "cycle-detected"
	EventClosing       EventKind = "closing"
	EventCleanupStart  EventKind = "cleanup-start"
	EventCleanupOK     EventKind = "cleanup-ok"
	EventCleanupFailed EventKind = "cleanup-failed"
	EventClosed        EventKind = "closed"
)

// Event is one lifecycle state change. It carries node ids and labels,
// never values or configuration. Which fields are set depends on Kind:
//
//   - registered: Scope, Overridden.
//   - resolve-start, resolve-ok, resolve-failed: Path, the label chain from
//     the root resolve, or from the nearest creator that has since returned,
//     to this token, so a later event's Path can be shorter than the start's;
//     ok and failed add Duration, ok adds Closer, failed adds ErrClass. ok
//     means the callers get the value: a creation that finishes during
//     Teardown's wait or after it is failed, with the class its callers get.
//   - cycle-detected: Path, the cycle's labels.
//   - closing: InFlight, the node ids of the creations still running.
//   - cleanup-start, cleanup-ok, cleanup-failed: Closer; ok and failed add
//     Duration, failed adds ErrClass.
//   - closed: InFlight, the creations still running; Remaining, the node
//     ids of the observer set, cleaned after Flush; HeldBack, the node ids
//     held back so far, for the late creations and for the cleanups Teardown
//     stopped waiting for; ErrorCount, the cleanup failures so far. An id is
//     in one list at most: a set member already held back is in HeldBack. closed
//     comes before the drainer stop and Flush, so when either of them overruns
//     and holds back the observer set, *TeardownTimeoutError's HeldBack lists
//     the set while closed lists it in Remaining.
type Event struct {
	// Seq numbers every event in the order it happened, dropped events
	// included, so a gap in the delivered sequence counts drops.
	Seq        uint64
	Kind       EventKind
	NodeID     string
	Label      string
	Scope      Scope
	Overridden bool
	Path       []string
	Duration   time.Duration
	ErrClass   string
	Closer     string
	InFlight   []string
	Remaining  []string
	HeldBack   []string
	ErrorCount int
}

// observerState is the container's side of its observer.
type observerState struct {
	observer  Observer
	inFactory bool
	disabled  bool

	queue []Event
	wake  chan struct{}

	inOnEvent     bool
	betweenEvents func() // test hook, run between deliveries
	stop          bool
	stopNow       bool
	abandoned     bool
	exited        chan struct{}
	closedEmitted bool

	pulled  map[Token]bool
	waiting map[Token]int
	set     map[*inst]bool
	frozen  bool
}

func (c *Container) startObserver() {
	o := &observerState{
		wake:    make(chan struct{}, 1),
		exited:  make(chan struct{}),
		pulled:  map[Token]bool{},
		waiting: map[Token]int{},
	}
	c.mu.Lock()
	c.obs = o
	o.inFactory = true
	c.mu.Unlock()
	var built Observer
	func() {
		defer func() {
			if p := recover(); p != nil {
				c.stats.observerPanics.Add(1)
				built = nil
			}
		}()
		built = c.obsFactory(&observerResolver{c: c})
	}()
	c.mu.Lock()
	o.inFactory = false
	o.observer = built
	if built == nil {
		o.disabled = true
		o.queue = nil
	}
	c.mu.Unlock()
	go c.drain()
}

// emitLocked appends e to the observer's queue. It never blocks: a full
// queue drops and counts the event, as does any event after closed.
func (c *Container) emitLocked(e Event) {
	o := c.obs
	if o == nil {
		return
	}
	c.seq++
	e.Seq = c.seq
	if o.closedEmitted {
		c.stats.postClosedEvents.Add(1)
		return
	}
	if o.disabled {
		return
	}
	if len(o.queue) >= eventQueueCap {
		c.stats.queueDrops.Add(1)
		return
	}
	o.queue = append(o.queue, e)
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (c *Container) dropBacklogLocked() {
	o := c.obs
	if n := len(o.queue); n > 0 {
		if !o.disabled {
			c.stats.queueDrops.Add(uint64(n))
		}
		o.queue = nil
	}
}

// drain is the observer's single delivery goroutine.
func (c *Container) drain() {
	o := c.obs
	defer close(o.exited)
	for {
		c.mu.Lock()
		if o.stopNow {
			c.dropBacklogLocked()
			c.mu.Unlock()
			return
		}
		if len(o.queue) > 0 {
			e := o.queue[0]
			o.queue[0] = Event{}
			o.queue = o.queue[1:]
			if o.disabled || o.observer == nil {
				c.mu.Unlock()
				continue
			}
			o.inOnEvent = true
			obs := o.observer
			c.mu.Unlock()
			panicked := deliver(obs, e)
			c.mu.Lock()
			o.inOnEvent = false
			if panicked {
				c.stats.observerPanics.Add(1)
				o.disabled = true
				o.queue = nil
			}
			abandoned := o.abandoned
			hook := o.betweenEvents
			c.mu.Unlock()
			if abandoned {
				c.participantDone()
				return
			}
			if hook != nil {
				hook()
			}
			continue
		}
		if o.stop {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		<-o.wake
	}
}

func deliver(obs Observer, e Event) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	obs.OnEvent(e)
	return false
}

func safeFlush(ctx context.Context, obs Observer) (panicked bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &panicError{value: p}
			panicked = true
		}
	}()
	return false, obs.Flush(ctx)
}

// observerResolver is the only cutout the observer holds. It enforces P16.
type observerResolver struct {
	c *Container
}

func (r *observerResolver) Resolve(ctx context.Context, t Token) (any, error) {
	return r.c.observerResolve(ctx, t, false)
}

func (r *observerResolver) GetCached(t Token) (any, error) {
	return r.c.observerResolve(context.Background(), t, true)
}

func (c *Container) refuseLocked(t Token, also error) error {
	c.stats.observerResolveErrors.Add(1)
	if also == nil {
		return fmt.Errorf("%w: %s", ErrObserverResolve, t.Label())
	}
	return fmt.Errorf("%w: %w: %s", ErrObserverResolve, also, t.Label())
}

func (c *Container) observerMemberLocked(t Token) bool {
	i := c.tokenInst[t]
	return i != nil && c.obs.set[i]
}

func (c *Container) observerResolve(ctx context.Context, t Token, cacheOnly bool) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.obs
	switch {
	case o.inFactory:
		return nil, c.refuseLocked(t, nil)
	case o.disabled:
		return nil, c.refuseLocked(t, ErrNotResolved)
	case c.state == stateClosed && c.slots[t] != nil:
		return nil, c.refuseLocked(t, ErrClosing)
	}
	s := c.slots[t]
	if s == nil {
		c.stats.observerResolveErrors.Add(1)
		err := notRegistered(t, c.state != stateOpen)
		if c.state == stateClosed {
			err = fmt.Errorf("%w: %w", ErrObserverResolve, err)
		}
		return nil, err
	}
	if s.core.scope == Transient {
		return nil, c.refuseLocked(t, nil)
	}
	if o.frozen && !c.observerMemberLocked(t) {
		return nil, c.refuseLocked(t, ErrClosing)
	}
	if s.cached != nil {
		o.pulled[t] = true
		return s.cached.value, nil
	}
	if cacheOnly {
		return nil, c.refuseLocked(t, ErrNotResolved)
	}
	cr := s.latch
	if cr == nil {
		if !c.appliedDone || c.state != stateOpen {
			return nil, c.refuseLocked(t, ErrNotResolved)
		}
		cr = c.startSingletonLocked(s, nil)
	}
	o.waiting[t]++
	cr.waiters++
	c.mu.Unlock()
	select {
	case <-cr.done:
	case <-ctx.Done():
	}
	if c.afterWake != nil {
		c.afterWake(t)
	}
	c.mu.Lock()
	cr.waiters--
	if o.waiting[t]--; o.waiting[t] <= 0 {
		delete(o.waiting, t)
	}
	select {
	case <-cr.done:
	default:
		c.stats.observerResolveErrors.Add(1)
		return nil, fmt.Errorf("typewirego: resolve %s: %w", t.Label(), ctx.Err())
	}
	v, err := cr.result()
	if err != nil {
		c.stats.observerResolveErrors.Add(1)
		return nil, err
	}
	if c.state == stateClosed {
		return nil, c.refuseLocked(t, ErrClosing)
	}
	if o.frozen && !c.observerMemberLocked(t) {
		return nil, c.refuseLocked(t, ErrClosing)
	}
	o.pulled[t] = true
	return v, nil
}

// observerSetLocked is the observer set: the tokens the observer's resolver
// returned or waits on, plus their runtime import closure, minus transients
// and held-back values.
func (c *Container) observerSetLocked() map[*inst]bool {
	o := c.obs
	if o == nil {
		return nil
	}
	if o.frozen {
		return o.set
	}
	roots := map[Token]bool{}
	for t := range o.pulled {
		roots[t] = true
	}
	for t := range o.waiting {
		roots[t] = true
	}
	set := map[*inst]bool{}
	for i := range c.closureLocked(roots, nil) {
		if i.scope == Singleton && !c.heldBack[i] && !i.late {
			set[i] = true
		}
	}
	return set
}

// observerTeardown runs the drainer stop, Flush and the observer-set pass,
// each bounded by one allowance. It reports whether the observer set was held
// back for an abandoned OnEvent or an overrunning Flush.
func (c *Container) observerTeardown(ctx context.Context, allowance time.Duration, errs *[]error, tt *TeardownTimeoutError) bool {
	o := c.obs
	late := context.WithoutCancel(ctx)

	c.mu.Lock()
	o.stop = true
	select {
	case o.wake <- struct{}{}:
	default:
	}
	c.mu.Unlock()
	hold := false
	skipFlush := false
	timer := time.NewTimer(allowance + cleanupGrace)
	select {
	case <-o.exited:
	case <-timer.C:
		c.mu.Lock()
		if o.inOnEvent {
			o.abandoned = true
			c.dropBacklogLocked()
			c.participants++
			c.stats.flushErrors.Add(1)
			skipFlush = true
			hold = true
			c.mu.Unlock()
		} else {
			// Between events: the drainer checks stopNow before it takes
			// another event, so it never calls OnEvent again.
			o.stopNow = true
			c.dropBacklogLocked()
			select {
			case o.wake <- struct{}{}:
			default:
			}
			c.mu.Unlock()
		}
	}
	timer.Stop()

	c.mu.Lock()
	obs := o.observer
	disabled := o.disabled
	c.mu.Unlock()
	if !skipFlush && !disabled && obs != nil {
		fctx, cancel := context.WithTimeout(late, allowance)
		var finished, overran bool
		done := make(chan struct{})
		go func() {
			defer cancel()
			panicked, err := safeFlush(fctx, obs)
			c.mu.Lock()
			finished = true
			if panicked {
				c.stats.observerPanics.Add(1)
				o.disabled = true
			}
			if err != nil && !overran {
				c.stats.flushErrors.Add(1)
			}
			ab := overran
			c.mu.Unlock()
			close(done)
			if ab {
				c.participantDone()
			}
		}()
		ft := time.NewTimer(allowance + cleanupGrace)
		select {
		case <-done:
		case <-ft.C:
			c.mu.Lock()
			if !finished {
				overran = true
				c.stats.flushErrors.Add(1)
				c.participants++
				hold = true
			}
			c.mu.Unlock()
		}
		ft.Stop()
	}

	c.mu.Lock()
	var list []*inst
	for i := range o.set {
		if !c.heldBack[i] && !i.cleaned {
			list = append(list, i)
		}
	}
	if hold && len(list) > 0 {
		for h := range c.closureLocked(nil, list) {
			if !h.cleaned {
				c.heldBack[h] = true
			}
		}
		c.mu.Unlock()
		return true
	}
	c.mu.Unlock()
	sortByIdxDesc(list)
	pctx, cancel := context.WithTimeout(late, allowance)
	defer cancel()
	c.runPass(pctx, list, errs, tt)
	return false
}

// errClass names an error's class for Event.ErrClass: the first matching
// sentinel, else the innermost error's type, never its text.
func errClass(err error) string {
	if err == nil {
		return ""
	}
	classes := []struct {
		target error
		name   string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{ErrCircularDependency, "cycle"},
		{ErrWireNotRegistered, "not_registered"},
		{ErrClosing, "closing"},
		{ErrTeardownTimeout, "teardown_timeout"},
		{ErrNotResolved, "not_resolved"},
		{ErrNotImported, "not_imported"},
		{ErrObserverResolve, "observer_resolve"},
		{ErrCreator, "creator"},
	}
	for _, cl := range classes {
		if errors.Is(err, cl.target) {
			return cl.name
		}
	}
	for {
		if u, ok := err.(interface{ Unwrap() error }); ok {
			if next := u.Unwrap(); next != nil {
				err = next
				continue
			}
		}
		if u, ok := err.(interface{ Unwrap() []error }); ok {
			if es := u.Unwrap(); len(es) > 0 && es[0] != nil {
				err = es[0]
				continue
			}
		}
		return fmt.Sprintf("%T", err)
	}
}
