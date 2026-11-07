package typewirego

import (
	"context"
	"fmt"
	"time"
)

// chainResolver is the Resolver a creator receives: it carries the
// creator's frame, so resolves through it extend the creator's chain.
type chainResolver struct {
	c  *Container
	fr *frame
}

func (r *chainResolver) Resolve(ctx context.Context, t Token) (any, error) {
	return r.c.resolve(ctx, r.fr, t)
}

func (r *chainResolver) GetCached(t Token) (any, error) {
	return r.c.getCached(t, r.fr)
}

// activeChain returns fr if its creator is still running, else nil: a chain
// ends when its creator returns.
func activeChain(fr *frame) *frame {
	if fr == nil || fr.ended {
		return nil
	}
	return fr
}

// chainFrames lists the active frames from the chain's root to fr.
func chainFrames(fr *frame) []*frame {
	var out []*frame
	for f := fr; f != nil && !f.ended; f = f.parent {
		out = append(out, f)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func chainLabels(fr *frame) []string {
	frames := chainFrames(fr)
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.label
	}
	return out
}

// framePath is the label chain from the root (or the nearest ended creator)
// to fr itself.
func framePath(fr *frame) []string {
	if fr.ended {
		return []string{fr.label}
	}
	return chainLabels(fr)
}

// waiterOf is the running singleton creation whose goroutine a resolve from
// chain blocks: the nearest singleton frame up the active chain.
func waiterOf(chain *frame) *inst {
	for f := chain; f != nil && !f.ended; f = f.parent {
		if f.inst != nil && f.inst.scope == Singleton {
			return f.inst
		}
	}
	return nil
}

// recordImportLocked records t as a runtime import of the creation that owns
// from, whether or not that creator is still running.
func (c *Container) recordImportLocked(from *frame, t Token) {
	i := from.inst
	if i == nil {
		return
	}
	if i.scope == Singleton {
		i.imports[t] = true
		return
	}
	m := c.transImports[i.token]
	if m == nil {
		m = map[Token]bool{}
		c.transImports[i.token] = m
	}
	m[t] = true
}

// checkRequiresLocked runs at a non-observer resolve: until every wire the
// observer requires is bound, it fails with ErrWireNotRegistered.
func (c *Container) checkRequiresLocked() error {
	if c.appliedDone {
		return nil
	}
	for _, w := range c.obsRequires {
		if c.slots[w.Token()] == nil {
			return fmt.Errorf("%w: %s (required by the observer)", ErrWireNotRegistered, w.Token().Label())
		}
	}
	c.appliedDone = true
	return nil
}

func (c *Container) cycleLocked(path []string) error {
	c.emitLocked(Event{Kind: EventCycleDetected, Path: path})
	return &CircularDependencyError{Path: path}
}

// waitCycleLocked walks the wait-for graph from target: if target transitively
// waits on a creation on the caller's chain, the wait would never end.
func (c *Container) waitCycleLocked(chain *frame, target *inst) []string {
	frames := chainFrames(chain)
	if len(frames) == 0 {
		return nil
	}
	mine := map[*inst]bool{}
	for _, f := range frames {
		if f.inst != nil && f.inst.scope == Singleton {
			mine[f.inst] = true
		}
	}
	prev := map[*inst]*inst{target: nil}
	queue := []*inst{target}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if mine[cur] {
			var tail []string
			for n := cur; n != nil; n = prev[n] {
				tail = append(tail, n.label)
			}
			for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
				tail[i], tail[j] = tail[j], tail[i]
			}
			return append(chainLabels(chain), tail...)
		}
		if cur.fr.ended {
			continue
		}
		for next := range cur.waits {
			if _, seen := prev[next]; !seen {
				prev[next] = cur
				queue = append(queue, next)
			}
		}
	}
	return nil
}

func (c *Container) resolve(ctx context.Context, from *frame, t Token) (any, error) {
	c.mu.Lock()
	chain := activeChain(from)
	if c.state != stateOpen {
		if from != nil && c.slots[t] != nil {
			c.recordImportLocked(from, t)
		}
		c.mu.Unlock()
		return nil, closingErr(t)
	}
	if err := c.checkRequiresLocked(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	s := c.slots[t]
	if s == nil {
		c.mu.Unlock()
		return nil, notRegistered(t)
	}
	if from != nil {
		c.recordImportLocked(from, t)
	}
	if chain != nil {
		for _, f := range chainFrames(chain) {
			if f.token == t {
				err := c.cycleLocked(append(chainLabels(chain), t.Label()))
				c.mu.Unlock()
				return nil, err
			}
		}
	}
	if s.core.scope == Transient {
		return c.runTransient(ctx, chain, s)
	}
	if s.cached != nil {
		v := s.cached.value
		c.mu.Unlock()
		return v, nil
	}
	cr := s.latch
	if cr == nil {
		cr = c.startSingletonLocked(s, chain)
	} else if path := c.waitCycleLocked(chain, cr); path != nil {
		err := c.cycleLocked(path)
		c.mu.Unlock()
		return nil, err
	}
	return c.awaitLocked(ctx, chain, cr)
}

// awaitLocked waits for cr on behalf of a resolve from chain; c.mu is held on
// entry and released on return.
func (c *Container) awaitLocked(ctx context.Context, chain *frame, cr *inst) (any, error) {
	waiter := waiterOf(chain)
	if waiter != nil {
		waiter.waits[cr]++
	}
	c.mu.Unlock()
	select {
	case <-cr.done:
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if waiter != nil {
		if waiter.waits[cr]--; waiter.waits[cr] <= 0 {
			delete(waiter.waits, cr)
		}
	}
	select {
	case <-cr.done:
		return cr.result()
	default:
		return nil, fmt.Errorf("typewirego: resolve %s: %w", cr.label, ctx.Err())
	}
}

func (c *Container) newInstLocked(s *slot, parent *frame) *inst {
	c.startSeq++
	i := &inst{
		slot:     s,
		core:     s.core,
		token:    s.token,
		label:    s.token.Label(),
		scope:    s.core.scope,
		keepOpen: s.core.keepOpen,
		startSeq: c.startSeq,
		started:  time.Now(),
		done:     make(chan struct{}),
		imports:  map[Token]bool{},
		waits:    map[*inst]int{},
	}
	i.fr = &frame{token: s.token, label: i.label, parent: parent, inst: i}
	if i.scope == Singleton {
		i.lifeCtx, i.lifeCancel = context.WithCancel(context.Background())
	} else {
		i.lifeCtx = c.transLife
		i.callerReturned = make(chan struct{})
	}
	c.inFlight[i] = true
	c.emitLocked(Event{Kind: EventResolveStart, NodeID: s.id, Label: i.label, Path: framePath(i.fr)})
	return i
}

func (c *Container) startSingletonLocked(s *slot, chain *frame) *inst {
	i := c.newInstLocked(s, chain)
	s.latch = i
	go c.runSingleton(i)
	return i
}

func (c *Container) runSingleton(i *inst) {
	var v any
	var err error
	normal := false
	defer func() {
		if !normal {
			if p := recover(); p != nil {
				err = &panicError{value: p}
			} else {
				err = errGoexit
			}
			v = nil
		}
		c.finish(i, v, err)
	}()
	v, err = i.core.create(c.abortCtx, Deps{life: i.lifeCtx}, &chainResolver{c: c, fr: i.fr})
	normal = true
}

// runTransient runs a transient creator on the caller's goroutine under the
// caller's ctx; c.mu is held on entry and released on return.
func (c *Container) runTransient(ctx context.Context, chain *frame, s *slot) (any, error) {
	i := c.newInstLocked(s, chain)
	c.mu.Unlock()
	defer close(i.callerReturned)
	var v any
	var err error
	normal := false
	func() {
		defer func() {
			if normal {
				return
			}
			v = nil
			if p := recover(); p != nil {
				err = &panicError{value: p}
				return
			}
			// runtime.Goexit: release the creation before the goroutine
			// exits.
			c.finish(i, nil, errGoexit)
		}()
		v, err = i.core.create(ctx, Deps{life: i.lifeCtx}, &chainResolver{c: c, fr: i.fr})
		normal = true
	}()
	c.finish(i, v, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	return i.result()
}
