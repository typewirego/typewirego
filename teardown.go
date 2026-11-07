package typewirego

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// finish records the outcome of a creation: cached, cleaned in the current
// pass, or cleaned by itself when it finished late.
func (c *Container) finish(i *inst, v any, err error) {
	c.mu.Lock()
	i.fr.ended = true
	delete(c.inFlight, i)
	clear(i.waits)
	s := i.slot
	if s.latch == i {
		s.latch = nil
	}
	dur := time.Since(i.started)
	path := framePath(i.fr)
	late := c.fixed
	if late {
		i.late = true
	}
	if err != nil {
		i.err = &CreatorError{Label: i.label, Cause: err}
		i.closing = c.state != stateOpen && !late
		c.emitLocked(Event{Kind: EventResolveFailed, NodeID: s.id, Label: i.label, Path: path,
			Duration: dur, ErrClass: errClass(i.err)})
		i.cancelLife()
		close(i.done)
		c.mu.Unlock()
		if late {
			c.participantDone()
		}
		return
	}
	i.value = v
	switch {
	case late:
		c.finishLateLocked(i)
	case c.state != stateOpen:
		c.finishClosingLocked(i)
	default:
		c.finishOpenLocked(i)
	}
	c.emitLocked(Event{Kind: EventResolveOK, NodeID: s.id, Label: i.label, Path: path,
		Duration: dur, Closer: c.reportedCloserLocked(i).String()})
	close(i.done)
	c.mu.Unlock()
	if late {
		go c.cleanLate(i)
	}
}

func (c *Container) reportedCloserLocked(i *inst) closerKind {
	if i.keepOpen {
		return closerNone
	}
	if key, ok := identity(i.value); ok && c.keepOpenVals[key] {
		return closerNone
	}
	return closerOf(i.value)
}

func (c *Container) appendCreatedLocked(i *inst) {
	i.idx = len(c.created)
	c.created = append(c.created, i)
}

func (c *Container) holdLocked(t Token, owner *inst) {
	m := c.transHolds[t]
	if m == nil {
		m = map[*inst]bool{}
		c.transHolds[t] = m
	}
	m[owner] = true
}

func (c *Container) finishOpenLocked(i *inst) {
	if i.scope == Transient {
		i.slot.createdOnce = true
		if key, ok := identity(i.value); ok {
			if o := c.owners[key]; o != nil {
				c.holdLocked(i.token, o)
			}
		}
		return
	}
	i.slot.cached = i
	c.tokenInst[i.token] = i
	c.appendCreatedLocked(i)
	c.claimLocked(i)
}

// finishClosingLocked handles a creation that finished during Teardown's
// wait: never cached, cleaned in this pass.
func (c *Container) finishClosingLocked(i *inst) {
	i.closing = true
	c.appendCreatedLocked(i)
	if i.scope == Singleton {
		c.tokenInst[i.token] = i
		c.claimLocked(i)
		return
	}
	kind := closerOf(i.value)
	i.closer = kind
	if i.keepOpen || kind == closerNone {
		i.owns = false
		return
	}
	key, ok := identity(i.value)
	if !ok {
		i.owns = true
		return
	}
	if o := c.owners[key]; o != nil || c.closingTrans[key] != nil || c.keepOpenVals[key] {
		i.owns = false
		return
	}
	i.owns = true
	c.closingTrans[key] = i
	c.holdLocked(i.token, i)
}

// claimLocked settles ownership of a singleton's value, for a creation that
// finished while open or during Teardown's wait.
func (c *Container) claimLocked(i *inst) {
	kind := closerOf(i.value)
	i.closer = kind
	if kind == closerNone {
		i.owns = false
		return
	}
	key, ok := identity(i.value)
	if !ok {
		i.owns = !i.keepOpen
		if i.keepOpen {
			i.closer = closerNone
		}
		return
	}
	owner := c.owners[key]
	if i.keepOpen {
		i.closer = closerNone
		i.owns = false
		c.keepOpenVals[key] = true
		if owner != nil {
			owner.owns = false
			owner.closer = closerNone
			i.owner = owner
			return
		}
		if ct := c.closingTrans[key]; ct != nil {
			ct.owns = false
			ct.closer = closerNone
		}
		c.owners[key] = i
		return
	}
	if owner != nil {
		i.owner = owner
		i.owns = false
		if c.keepOpenVals[key] {
			i.closer = closerNone
		}
		return
	}
	if ct := c.closingTrans[key]; ct != nil && ct.owns {
		// A singleton finishing during the wait with a closing transient's
		// value takes it over, together with the transient's token.
		ct.owns = false
		delete(c.closingTrans, key)
		if m := c.transHolds[ct.token]; m != nil {
			delete(m, ct)
		}
		c.holdLocked(ct.token, i)
	}
	c.owners[key] = i
	i.owns = true
}

// finishLateLocked settles a creation that finished after Teardown's wait
// timed out: it cleans its own value unless another creation owns it.
func (c *Container) finishLateLocked(i *inst) {
	kind := closerOf(i.value)
	i.closer = kind
	i.owns = false
	if kind == closerNone {
		return
	}
	key, ok := identity(i.value)
	if !ok {
		i.owns = !i.keepOpen
		return
	}
	owner := c.owners[key]
	if i.keepOpen {
		i.closer = closerNone
		if owner != nil {
			i.owner = owner
		} else if i.scope == Singleton {
			c.keepOpenVals[key] = true
		}
		return
	}
	if owner != nil {
		i.owner = owner
		return
	}
	if c.keepOpenVals[key] {
		i.closer = closerNone
		return
	}
	c.owners[key] = i
	i.owns = true
}

// cleanLate cleans a late creation's own value, off its caller's goroutine
// and under a context Teardown's cancellation never reaches.
func (c *Container) cleanLate(i *inst) {
	if i.callerReturned != nil {
		<-i.callerReturned
	}
	c.cleanOne(context.Background(), i, &c.waitErrs)
	c.participantDone()
}

// cleanOne runs i's cleanup inline and records it; errs is guarded by c.mu.
func (c *Container) cleanOne(ctx context.Context, i *inst, errs *[]error) {
	c.mu.Lock()
	if i.cleaned {
		c.mu.Unlock()
		return
	}
	if !i.owns || i.closer == closerNone {
		c.slotPassedLocked(i)
		c.mu.Unlock()
		return
	}
	c.emitCleanupLocked(EventCleanupStart, i, 0, nil)
	c.mu.Unlock()
	start := time.Now()
	err := c.callCloser(ctx, i)
	c.mu.Lock()
	c.cleanupDoneLocked(i, err, time.Since(start), errs)
	c.mu.Unlock()
}

func (c *Container) emitCleanupLocked(kind EventKind, i *inst, d time.Duration, err error) {
	c.emitLocked(Event{Kind: kind, NodeID: i.slot.id, Label: i.label, Closer: i.closer.String(),
		Duration: d, ErrClass: errClass(err)})
}

func (c *Container) cleanupDoneLocked(i *inst, err error, d time.Duration, errs *[]error) {
	if err != nil {
		c.emitCleanupLocked(EventCleanupFailed, i, d, err)
		*errs = append(*errs, fmt.Errorf("typewirego: cleanup %s: %w", i.label, err))
	} else {
		c.emitCleanupLocked(EventCleanupOK, i, d, nil)
	}
	c.slotPassedLocked(i)
}

// slotPassedLocked marks i cleaned and ends its Lifetime, or, for a value
// another creation owns, defers that to the owner's cleanup.
func (c *Container) slotPassedLocked(i *inst) {
	i.cleaned = true
	if i.owner != nil && !i.owner.cleaned {
		i.owner.aliasDeps = append(i.owner.aliasDeps, i)
	} else {
		i.cancelLife()
	}
	for _, a := range i.aliasDeps {
		a.cancelLife()
	}
	i.aliasDeps = nil
}

func (c *Container) callCloser(ctx context.Context, i *inst) (err error) {
	defer func() {
		if p := recover(); p != nil {
			c.stats.cleanupPanics.Add(1)
			err = &panicError{value: p}
		}
	}()
	switch v := i.value.(type) {
	case interface {
		Shutdown(context.Context) error
	}:
		return v.Shutdown(ctx)
	case interface{ Close() error }:
		return v.Close()
	case interface{ Close() }:
		v.Close()
	}
	return nil
}

// closureLocked is the runtime import closure of tokens, as creations: each
// token's singleton, a transient token's imports and the owners it handed
// out, and the owner of every alias reached.
func (c *Container) closureLocked(tokens map[Token]bool, insts []*inst) map[*inst]bool {
	out := map[*inst]bool{}
	seen := map[Token]bool{}
	var visitTok func(Token)
	var visitInst func(*inst)
	visitInst = func(i *inst) {
		if out[i] {
			return
		}
		out[i] = true
		for t := range i.imports {
			visitTok(t)
		}
		if i.owner != nil {
			visitInst(i.owner)
		}
		if i.scope == Transient {
			visitTok(i.token)
		}
	}
	visitTok = func(t Token) {
		if seen[t] {
			return
		}
		seen[t] = true
		if i := c.tokenInst[t]; i != nil {
			visitInst(i)
		}
		for t2 := range c.transImports[t] {
			visitTok(t2)
		}
		for o := range c.transHolds[t] {
			visitInst(o)
		}
	}
	for t := range tokens {
		visitTok(t)
	}
	for _, i := range insts {
		visitInst(i)
	}
	return out
}

func sortByIdxDesc(list []*inst) {
	sort.Slice(list, func(a, b int) bool { return list[a].idx > list[b].idx })
}

func (c *Container) labelsLocked(set map[*inst]bool) []string {
	list := make([]*inst, 0, len(set))
	for i := range set {
		list = append(list, i)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].idx < list[b].idx })
	out := make([]string, len(list))
	for k, i := range list {
		out[k] = i.label
	}
	return out
}

func idsOf(set map[*inst]bool) []string {
	seen := map[*slot]bool{}
	var slots []*slot
	for i := range set {
		if !seen[i.slot] {
			seen[i.slot] = true
			slots = append(slots, i.slot)
		}
	}
	sort.Slice(slots, func(a, b int) bool { return slots[a].ordinal < slots[b].ordinal })
	out := make([]string, len(slots))
	for k, s := range slots {
		out[k] = s.id
	}
	return out
}

func (c *Container) inFlightIDsLocked() []string {
	set := map[*inst]bool{}
	for i := range c.inFlight {
		set[i] = true
	}
	return idsOf(set)
}

// Teardown closes the container. It enters Closing, so every later resolve
// fails with ErrClosing; cancels the context in-flight singleton creators run
// under; and waits for in-flight creations, bounded by ctx. A creation that
// finishes during the wait is never cached and never returned, so its value,
// singleton or transient, is cleaned in this pass. Then it cleans every
// created singleton value in reverse creation order, once, and returns the
// cleanup errors joined. Transient values that reached their callers are never
// closed.
//
// Each cleanup call runs on its own goroutine (inline when ctx can never end),
// and the pass waits on its calls until one short grace after ctx ends. A call
// still running then, or due to start after it, is left to finish on its own:
// its import closure is held back from every later pass, the next value is
// cleaned, and Teardown returns *TeardownTimeoutError listing the call in
// Cleanups. Wait reports its result.
//
// With an observer, the observer set (P17) is left out of that pass. After it
// come the closed event, the drainer stop, Flush and then the observer set's
// cleanup, each bounded by its own Flush allowance (P20). Observer failures
// are counted in Stats and never returned, except that a drainer abandoned
// inside OnEvent, or a Flush still running past its allowance, holds back the
// observer set until it returns, and Teardown returns *TeardownTimeoutError
// listing the set in HeldBack.
//
// If ctx ends while creations are still running, Teardown still runs that
// whole path, holding back the values the running creations import from both
// passes, and returns a *TeardownTimeoutError joined with the cleanup errors.
// Each late creation then cleans its own value when it finishes, and the
// held-back values are cleaned once after the last late creation or call;
// Wait reports those errors.
//
// Teardown calls are serialized; later calls wait for the first and return
// nil.
func (c *Container) Teardown(ctx context.Context) error {
	c.mu.Lock()
	if c.tdStarted {
		done := c.tdDone
		c.tdQueued++
		c.mu.Unlock()
		<-done
		return nil
	}
	c.tdStarted = true
	c.state = stateClosing
	c.participants = 1
	allowance := flushBudgetFor(ctx, c.flushBudget, time.Now())
	waiting := make([]*inst, 0, len(c.inFlight))
	for i := range c.inFlight {
		waiting = append(waiting, i)
	}
	c.emitLocked(Event{Kind: EventClosing, InFlight: c.inFlightIDsLocked()})
	c.mu.Unlock()
	c.abortCancel()

wait:
	for _, i := range waiting {
		select {
		case <-i.done:
		case <-ctx.Done():
			break wait
		}
	}

	c.mu.Lock()
	c.fixed = true
	for key, ct := range c.closingTrans {
		if ct.owns && c.owners[key] == nil {
			c.owners[key] = ct
		}
	}
	running := make([]*inst, 0, len(c.inFlight))
	for i := range c.inFlight {
		i.late = true
		running = append(running, i)
	}
	sort.Slice(running, func(a, b int) bool { return running[a].startSeq < running[b].startSeq })
	tt := &TeardownTimeoutError{}
	for _, i := range running {
		tt.Running = append(tt.Running, i.label)
	}
	c.stats.lateCreations.Add(uint64(len(running)))
	c.participants += len(running)
	roots := map[Token]bool{}
	for _, i := range running {
		if i.scope == Singleton {
			for t := range i.imports {
				roots[t] = true
			}
		} else {
			roots[i.token] = true
		}
	}
	for i := range c.closureLocked(roots, nil) {
		c.heldBack[i] = true
	}
	if c.obs != nil {
		c.obs.set = c.observerSetLocked()
		c.obs.frozen = true
	}
	main := make([]*inst, 0, len(c.created))
	for _, i := range c.created {
		if !c.heldBack[i] && (c.obs == nil || !c.obs.set[i]) {
			main = append(main, i)
		}
	}
	sortByIdxDesc(main)
	c.mu.Unlock()

	var errs []error
	c.runPass(ctx, main, &errs, tt)

	c.mu.Lock()
	closed := Event{Kind: EventClosed, InFlight: c.inFlightIDsLocked(), HeldBack: idsOf(c.heldBack),
		ErrorCount: len(errs)}
	if c.obs != nil {
		closed.Remaining = idsOf(c.obs.set)
	}
	c.emitLocked(closed)
	c.state = stateClosed
	if c.obs != nil {
		c.obs.closedEmitted = true
	}
	c.mu.Unlock()

	obsHeld := false
	if c.obs != nil {
		obsHeld = c.observerTeardown(ctx, allowance, &errs, tt)
	}

	c.mu.Lock()
	tt.HeldBack = c.labelsLocked(c.heldBack)
	c.mu.Unlock()
	var out []error
	if len(tt.Running) > 0 || len(tt.Cleanups) > 0 || obsHeld {
		out = append(out, tt)
	}
	out = append(out, errs...)
	c.participantDone()
	close(c.tdDone)
	return errors.Join(out...)
}

// cleanupCall is one cleanup call run on its own goroutine.
type cleanupCall struct {
	done      chan struct{}
	err       error
	finished  bool
	abandoned bool
}

// runPass cleans list in order, bounded by bound plus one shared grace.
func (c *Container) runPass(bound context.Context, list []*inst, errs *[]error, tt *TeardownTimeoutError) {
	inline := bound.Done() == nil
	var deadline time.Time
	fixed := false
	if !inline && bound.Err() != nil {
		fixed = true
		deadline = time.Now().Add(cleanupGrace)
	}
	for _, i := range list {
		c.mu.Lock()
		if c.heldBack[i] || i.cleaned {
			c.mu.Unlock()
			continue
		}
		if !i.owns || i.closer == closerNone {
			c.slotPassedLocked(i)
			c.mu.Unlock()
			continue
		}
		c.emitCleanupLocked(EventCleanupStart, i, 0, nil)
		c.mu.Unlock()
		start := time.Now()
		if inline {
			err := c.callCloser(bound, i)
			c.mu.Lock()
			c.cleanupDoneLocked(i, err, time.Since(start), errs)
			c.mu.Unlock()
			continue
		}
		call := &cleanupCall{done: make(chan struct{})}
		if fixed && !time.Now().Before(deadline) {
			// Due to start after the pass's deadline: start it, but never
			// wait for it.
			c.mu.Lock()
			c.abandonLocked(call, i, tt)
			c.mu.Unlock()
		}
		go func(i *inst) {
			err := c.callCloser(bound, i)
			c.mu.Lock()
			call.finished = true
			call.err = err
			abandoned := call.abandoned
			if abandoned {
				c.cleanupDoneLocked(i, err, time.Since(start), &c.waitErrs)
			}
			c.mu.Unlock()
			close(call.done)
			if abandoned {
				c.participantDone()
			}
		}(i)
		if call.abandoned {
			continue
		}
		waitCall(call.done, bound, &fixed, &deadline)
		c.mu.Lock()
		if call.finished {
			c.cleanupDoneLocked(i, call.err, time.Since(start), errs)
		} else {
			c.abandonLocked(call, i, tt)
		}
		c.mu.Unlock()
	}
}

// abandonLocked leaves a cleanup call to finish on its own: it is listed in
// Cleanups and its runtime import closure is held back from every later pass.
func (c *Container) abandonLocked(call *cleanupCall, i *inst, tt *TeardownTimeoutError) {
	call.abandoned = true
	c.participants++
	tt.Cleanups = append(tt.Cleanups, i.label)
	for h := range c.closureLocked(nil, []*inst{i}) {
		if h != i && !h.cleaned {
			c.heldBack[h] = true
		}
	}
}

// waitCall waits for done until one grace after bound ends; the deadline is
// shared by every call of a pass.
func waitCall(done <-chan struct{}, bound context.Context, fixed *bool, deadline *time.Time) {
	for {
		if *fixed {
			remaining := time.Until(*deadline)
			if remaining <= 0 {
				return
			}
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
			}
			return
		}
		select {
		case <-done:
			return
		case <-bound.Done():
			*fixed = true
			*deadline = time.Now().Add(cleanupGrace)
		}
	}
}

// participantDone ends one participant of a Teardown: Teardown itself, a late
// creation, or a call Teardown stopped waiting for. The last one cleans the
// held-back values, off the caller's goroutine, under a context Teardown's
// cancellation never reaches.
func (c *Container) participantDone() {
	c.mu.Lock()
	c.participants--
	if c.participants > 0 {
		c.mu.Unlock()
		return
	}
	var list []*inst
	for i := range c.heldBack {
		if !i.cleaned {
			list = append(list, i)
		}
	}
	sortByIdxDesc(list)
	c.mu.Unlock()
	if len(list) == 0 {
		c.finishAll()
		return
	}
	go func() {
		for _, i := range list {
			c.cleanOne(context.Background(), i, &c.waitErrs)
		}
		c.finishAll()
	}()
}

func (c *Container) finishAll() {
	c.transLifeCancel()
	close(c.allDone)
}

// Wait blocks until every cleanup the container owns has run: after a Teardown
// that finished every call itself that is at once; otherwise, once the late
// creations, the cleanup, OnEvent and Flush calls Teardown stopped waiting for
// and the held-back values are done. It returns the cleanup errors Teardown
// did not return, joined, or ctx.Err() if ctx ends first. Flush results are
// never among them; they are counted in Stats.
func (c *Container) Wait(ctx context.Context) error {
	select {
	case <-c.allDone:
		c.mu.Lock()
		defer c.mu.Unlock()
		return errors.Join(c.waitErrs...)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// flushBudgetFor is the Flush allowance fixed when Teardown begins: the
// configured budget, or a quarter of the time left on ctx if less.
func flushBudgetFor(ctx context.Context, configured time.Duration, now time.Time) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		left := dl.Sub(now)
		if left < 0 {
			left = 0
		}
		if q := left / 4; q < configured {
			return q
		}
	}
	return configured
}
