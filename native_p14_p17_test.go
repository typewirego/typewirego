package typewirego_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/typewirego/typewirego"
)

func TestP14ObserverFactoryReceivesResolverOnly(t *testing.T) {
	var got typewirego.Resolver
	c := typewirego.NewContainer(bg(), typewirego.WithObserver(func(r typewirego.Resolver) typewirego.Observer {
		got = r
		return &recObserver{}
	}))
	defer c.Teardown(bg())
	if got == nil {
		t.Fatal("factory did not run inside NewContainer")
	}
	if _, ok := got.(*typewirego.Container); ok {
		t.Fatal("the observer received the container itself")
	}
	if _, ok := got.(typewirego.Binder); ok {
		t.Fatal("the observer's resolver is also a Binder")
	}
}

func TestP14ObserverPanicIsRecoveredCountedAndDisables(t *testing.T) {
	w := leafWire("t.W")
	c, o := observed(func(o *recObserver) {
		o.onEvent = func(typewirego.Event) { panic("observer bug") }
	})
	apply(t, c, w)
	eventually(t, "the panic to be counted", func() bool { return c.Stats().ObserverPanics == 1 })
	if v := mustResolve(t, c, w); v != "t.W" {
		t.Fatalf("resolve after an observer panic = %v", v)
	}
	// Open and cached: an enabled observer would get a cache hit here.
	if _, err := o.r.Resolve(bg(), w.Token()); !errors.Is(err, typewirego.ErrObserverResolve) {
		t.Fatalf("disabled observer's Resolve = %v, want refused", err)
	}
	if _, err := o.r.GetCached(w.Token()); !errors.Is(err, typewirego.ErrObserverResolve) {
		t.Fatalf("disabled observer's GetCached = %v, want refused", err)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	if n := len(o.got()); n != 1 {
		t.Fatalf("observer received %d events after it panicked, want only the first", n)
	}
	if o.flushed.Load() != 0 {
		t.Fatal("a disabled observer was flushed")
	}
}

func TestP14ObserverErrorNeverAltersResolveOrTeardown(t *testing.T) {
	log := &orderLog{}
	w := resWire("t.W", log)
	fresh := leafWire("t.Fresh")
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { return errors.New("flush failed") }
	})
	apply(t, c, w, fresh)
	first := mustResolve(t, c, w)
	// Observer resolve errors are the observer's, not the caller's.
	if _, err := o.r.Resolve(bg(), leafWire("t.Unbound").Token()); err == nil {
		t.Fatal("unbound observer resolve succeeded")
	}
	if _, err := o.r.Resolve(bg(), typewirego.Of(typewirego.Opts[int]{Token: "t.Tr", Scope: typewirego.Transient,
		Creator: func(context.Context) (int, error) { return 0, nil }}).Token()); err == nil {
		t.Fatal("observer resolve of an unbound transient succeeded")
	}
	if v, err := c.Resolve(bg(), w.Token()); err != nil || v != first {
		t.Fatalf("app resolve after observer errors = %v, %v", v, err)
	}
	if v, err := c.Resolve(bg(), fresh.Token()); err != nil || v != "t.Fresh" {
		t.Fatalf("fresh app resolve after observer errors = %v, %v", v, err)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatalf("teardown = %v, want nil despite observer failures", err)
	}
	s := c.Stats()
	if s.FlushErrors != 1 || s.ObserverResolveErrors != 2 {
		t.Fatalf("stats = %+v", s)
	}
	if log.count("t.W") != 1 {
		t.Fatal("cleanup did not run")
	}
}

func TestP14FactoryResolvingDuringNewContainerIsRecorded(t *testing.T) {
	sink := leafWire("t.Sink")
	var factoryErr error
	c := typewirego.NewContainer(bg(), typewirego.WithObserver(func(r typewirego.Resolver) typewirego.Observer {
		_, factoryErr = r.Resolve(bg(), sink.Token())
		return &recObserver{}
	}))
	defer c.Teardown(bg())
	if !errors.Is(factoryErr, typewirego.ErrObserverResolve) || errors.Is(factoryErr, typewirego.ErrNotResolved) {
		t.Fatalf("factory resolve = %v, want the NewContainer refusal (not a cache miss)", factoryErr)
	}
	if c.Stats().ObserverResolveErrors != 1 {
		t.Fatalf("ObserverResolveErrors = %d", c.Stats().ObserverResolveErrors)
	}
	apply(t, c, sink)
	mustResolve(t, c, sink)
	if got := pulledLabels(c); len(got) != 0 {
		t.Fatalf("the factory's refused resolve joined the observer set: %v", got)
	}
}

func TestP15ObserverPullsSinkInsideFirstOnEvent(t *testing.T) {
	sink := leafWire("t.Sink")
	app := leafWire("t.App")
	ready := make(chan struct{})
	var first sync.Once
	var pullErr error
	deliveredAtPull := -1
	pulled := make(chan struct{})
	c, o := observedWith(func(o *recObserver) {
		o.onEvent = func(e typewirego.Event) {
			first.Do(func() {
				<-ready
				_, pullErr = o.r.Resolve(bg(), sink.Token())
				deliveredAtPull = len(o.got())
				close(pulled)
			})
		}
	}, []typewirego.AnyWire{sink})
	apply(t, c, app, sink)
	mustResolve(t, c, app)
	close(ready)
	waitClosed(t, pulled, "the pull")
	if pullErr != nil {
		t.Fatalf("pull inside the first OnEvent = %v", pullErr)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if o.overlap.Load() {
		t.Fatal("OnEvent was re-entered")
	}
	idx := -1
	for i, e := range o.got() {
		if e.Kind == typewirego.EventResolveStart && e.Label == "t.Sink" {
			idx = i
			break
		}
	}
	if deliveredAtPull != 1 || idx < deliveredAtPull {
		t.Fatalf("the pull's events (index %d) must be delivered after the OnEvent that caused them (%d delivered then)", idx, deliveredAtPull)
	}
}

func TestP15EventsDeliveredInSequenceNeverReentrant(t *testing.T) {
	lazy := leafWire("t.Lazy")
	var once sync.Once
	var pullErr error
	c, o := observed(nil)
	o.mu.Lock()
	o.onEvent = func(e typewirego.Event) {
		if e.Kind == typewirego.EventResolveOK && e.Label != "t.Lazy" {
			// Creating a value from inside OnEvent queues its events; they
			// must not be delivered before this OnEvent returns.
			once.Do(func() { _, pullErr = o.r.Resolve(bg(), lazy.Token()) })
		}
	}
	o.mu.Unlock()
	var ws []typewirego.AnyWire
	for i := range 20 {
		ws = append(ws, leafWire(fmt.Sprintf("t.W%d", i)))
	}
	apply(t, c, append(ws, lazy)...)
	for _, w := range ws {
		mustResolve(t, c, w)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if pullErr != nil {
		t.Fatalf("pull from OnEvent = %v", pullErr)
	}
	if o.overlap.Load() {
		t.Fatal("OnEvent ran concurrently or re-entrantly")
	}
	if o.find(typewirego.EventResolveOK, "t.Lazy") == nil {
		t.Fatal("the events the pull caused were never delivered")
	}
	events := o.got()
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("event %d has Seq %d after %d", i, events[i].Seq, events[i-1].Seq)
		}
	}
	if events[len(events)-1].Kind != typewirego.EventClosed {
		t.Fatalf("last event = %s, want closed", events[len(events)-1].Kind)
	}
}

func TestP15FullQueueDropsAndCountsWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	c, o := observed(func(o *recObserver) {
		o.onEvent = func(typewirego.Event) { once.Do(func() { <-release }) }
	})
	tr := typewirego.Of(typewirego.Opts[int]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (int, error) { return 1, nil }})
	apply(t, c, tr)
	// Each transient resolve emits two events, so this overfills the queue
	// while the drainer is stuck in its first OnEvent. None of it may block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range typewirego.EventQueueCap {
			if _, err := c.Resolve(bg(), tr.Token()); err != nil {
				t.Error(err)
			}
		}
	}()
	waitClosed(t, done, "resolves while the observer is stuck")
	if c.Stats().QueueDrops == 0 {
		t.Fatal("a full queue dropped nothing")
	}
	close(release)
	const emitted = 1 + 2*typewirego.EventQueueCap
	eventually(t, "the backlog to drain", func() bool {
		return uint64(len(o.got()))+c.Stats().QueueDrops == emitted
	})
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	events := o.got()
	last := events[len(events)-1]
	if last.Kind != typewirego.EventClosed {
		t.Fatalf("last delivered = %s", last.Kind)
	}
	if gaps := last.Seq - uint64(len(events)); gaps != c.Stats().QueueDrops {
		t.Fatalf("sequence gaps = %d, QueueDrops = %d", gaps, c.Stats().QueueDrops)
	}
}

func TestP15ResolveOkPrecedesEventsUsingCachedValue(t *testing.T) {
	g := newGate()
	s := typewirego.Of(typewirego.Opts[string]{Token: "t.S", Creator: func(context.Context) (string, error) {
		g.enter()
		return "s", nil
	}})
	user := typewirego.Of(typewirego.Opts[string]{Token: "t.User", Scope: typewirego.Transient,
		Imports: []typewirego.Import{{Name: "s", Wire: s}},
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (string, error) {
			return typewirego.Dep(d, s)
		}})
	c, o := observed(nil)
	apply(t, c, user)
	const n = 4
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			// Each waiter wakes on the latch, then uses the cached value.
			if _, err := c.Resolve(bg(), s.Token()); err != nil {
				t.Error(err)
			}
			if _, err := c.Resolve(bg(), user.Token()); err != nil {
				t.Error(err)
			}
		})
	}
	waitClosed(t, g.started, "creator")
	eventually(t, "every waiter on the latch", func() bool { return typewirego.LatchWaiters(c, s.Token()) == n })
	g.open()
	wg.Wait()
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	ok := o.find(typewirego.EventResolveOK, "t.S")
	if ok == nil {
		t.Fatal("no resolve-ok for t.S")
	}
	users := 0
	for _, e := range o.got() {
		if e.Label == "t.User" && e.Kind != typewirego.EventRegistered {
			users++
			if e.Seq < ok.Seq {
				t.Fatalf("%s for t.User (seq %d) precedes t.S resolve-ok (seq %d)", e.Kind, e.Seq, ok.Seq)
			}
		}
	}
	if users != 2*n {
		t.Fatalf("%d t.User events, want %d", users, 2*n)
	}
}

func TestP15ClosingPrecedesCleanupAndClosedIsLast(t *testing.T) {
	log := &orderLog{}
	a, b := resWire("t.A", log), resWire("t.B", log)
	c, o := observed(nil)
	apply(t, c, a, b)
	mustResolve(t, c, a)
	mustResolve(t, c, b)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	kinds := o.kinds()
	closing := slices.Index(kinds, typewirego.EventClosing)
	firstCleanup := slices.Index(kinds, typewirego.EventCleanupStart)
	if closing < 0 || firstCleanup < 0 || closing > firstCleanup {
		t.Fatalf("closing must precede cleanup: %v", kinds)
	}
	if kinds[len(kinds)-1] != typewirego.EventClosed {
		t.Fatalf("closed is not last: %v", kinds)
	}
	if n := slices.Index(kinds, typewirego.EventClosed); n != len(kinds)-1 {
		t.Fatalf("more than one closed, or events after it: %v", kinds)
	}
}

func TestP15OnEventNeverOverlapsFlush(t *testing.T) {
	release := make(chan struct{})
	var delivered atomic.Int32
	var flushSaw int32
	c, o := observed(func(o *recObserver) {
		o.onEvent = func(e typewirego.Event) {
			delivered.Add(1)
			if e.Kind == typewirego.EventClosing {
				<-release // a backlog builds up behind this event
			}
		}
		o.flush = func(context.Context) error {
			flushSaw = delivered.Load()
			return nil
		}
	})
	releaser := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.Releaser", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error { close(release); return nil }}, nil
	}})
	var ws []typewirego.AnyWire
	for i := range 20 {
		ws = append(ws, resWire(fmt.Sprintf("t.W%d", i), &orderLog{}))
	}
	apply(t, c, append(ws, releaser)...)
	mustResolve(t, c, releaser)
	for _, w := range ws {
		mustResolve(t, c, w)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if o.overlap.Load() {
		t.Fatal("Flush overlapped OnEvent")
	}
	if n := int32(len(o.got())); flushSaw != n || o.find(typewirego.EventClosed, "") == nil {
		t.Fatalf("Flush ran after %d of %d events; the backlog, closed included, must be delivered first", flushSaw, n)
	}
}

func TestP15EventOrderUnderConcurrentResolves(t *testing.T) {
	c, o := observed(nil)
	var ws []typewirego.AnyWire
	for i := range 16 {
		ws = append(ws, leafWire(fmt.Sprintf("t.W%d", i)))
	}
	apply(t, c, ws...)
	var wg sync.WaitGroup
	for range 4 {
		for _, w := range ws {
			wg.Go(func() {
				if _, err := c.Resolve(bg(), w.Token()); err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	events := o.got()
	starts, oks := map[string]uint64{}, map[string]uint64{}
	for i, e := range events {
		if i > 0 && e.Seq <= events[i-1].Seq {
			t.Fatal("events delivered out of sequence")
		}
		switch e.Kind {
		case typewirego.EventResolveStart:
			if _, dup := starts[e.NodeID]; dup {
				t.Fatalf("two creations of singleton %s", e.Label)
			}
			starts[e.NodeID] = e.Seq
		case typewirego.EventResolveOK:
			oks[e.NodeID] = e.Seq
		}
	}
	for id, s := range starts {
		if oks[id] <= s {
			t.Fatalf("node %s: resolve-ok (seq %d) not after resolve-start (seq %d)", id, oks[id], s)
		}
	}
	if len(starts) != len(ws) {
		t.Fatalf("%d creations, want %d", len(starts), len(ws))
	}
}

func TestP16ObserverResolverRejectsTransients(t *testing.T) {
	var calls atomic.Int32
	tr := typewirego.Of(typewirego.Opts[int]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (int, error) { calls.Add(1); return 1, nil }})
	app := leafWire("t.App")
	c, o := observed(nil)
	defer c.Teardown(bg())
	apply(t, c, tr, app)
	mustResolve(t, c, app)
	if _, err := o.r.Resolve(bg(), tr.Token()); !errors.Is(err, typewirego.ErrObserverResolve) {
		t.Fatalf("observer resolve of a transient = %v", err)
	}
	if _, err := o.r.GetCached(tr.Token()); !errors.Is(err, typewirego.ErrObserverResolve) {
		t.Fatalf("observer GetCached of a transient = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("the observer ran a transient creator")
	}
}

func TestP16ObserverResolverServesCacheHitsOnlyDuringApply(t *testing.T) {
	sink := leafWire("t.Sink")
	app := leafWire("t.App")
	c, o := observed(nil)
	defer c.Teardown(bg())
	apply(t, c, sink, app)
	// Before the first resolve that is not the observer's, the observer may
	// only read the cache, and nothing is cached yet.
	_, err := o.r.Resolve(bg(), sink.Token())
	if !errors.Is(err, typewirego.ErrObserverResolve) || !errors.Is(err, typewirego.ErrNotResolved) {
		t.Fatalf("observer resolve during Apply = %v", err)
	}
	if c.Graph().Nodes[0].Created {
		t.Fatal("the observer created a value during Apply")
	}
	mustResolve(t, c, app)
	if v, err := o.r.Resolve(bg(), sink.Token()); err != nil || v != "t.Sink" {
		t.Fatalf("observer resolve after the first resolve = %v, %v", v, err)
	}
}

func TestP16ObserverResolverServesCacheHitsOnlyWhileClosing(t *testing.T) {
	sink := leafWire("t.Sink")
	never := leafWire("t.Never")
	g := newGate()
	slow := typewirego.Of(typewirego.Opts[string]{Token: "t.Slow", Creator: func(context.Context) (string, error) {
		g.enter()
		return "slow", nil
	}})
	c, o := observed(nil)
	apply(t, c, sink, never, slow)
	mustResolve(t, c, sink)
	go c.Resolve(bg(), slow.Token())
	waitClosed(t, g.started, "slow creator")
	done := make(chan error, 1)
	go func() { done <- c.Teardown(bg()) }()
	eventually(t, "closing", func() bool {
		_, err := c.GetCached(sink.Token())
		return errors.Is(err, typewirego.ErrClosing)
	})
	if v, err := o.r.Resolve(bg(), sink.Token()); err != nil || v != "t.Sink" {
		t.Fatalf("observer cache hit while closing = %v, %v", v, err)
	}
	_, err := o.r.Resolve(bg(), never.Token())
	if !errors.Is(err, typewirego.ErrObserverResolve) || !errors.Is(err, typewirego.ErrNotResolved) {
		t.Fatalf("observer cache miss while closing = %v", err)
	}
	g.open()
	if err := recv(t, done, "teardown"); err != nil {
		t.Fatal(err)
	}
}

func TestP16ObserverResolverFailsAfterClosed(t *testing.T) {
	sink := leafWire("t.Sink")
	c, o := observed(nil)
	apply(t, c, sink)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() (any, error){
		func() (any, error) { return o.r.Resolve(bg(), sink.Token()) },
		func() (any, error) { return o.r.GetCached(sink.Token()) },
	} {
		if _, err := f(); !errors.Is(err, typewirego.ErrObserverResolve) || !errors.Is(err, typewirego.ErrClosing) {
			t.Fatalf("observer resolve after closed = %v", err)
		}
	}
}

func TestP16ObserverPullWaitsOnAnotherGoroutinesLatch(t *testing.T) {
	g := newGate()
	var calls atomic.Int32
	sink := typewirego.Of(typewirego.Opts[*int]{Token: "t.Sink", Creator: func(context.Context) (*int, error) {
		calls.Add(1)
		g.enter()
		return new(int), nil
	}})
	c, o := observed(nil)
	defer c.Teardown(bg())
	apply(t, c, sink)
	appGot := make(chan any, 1)
	go func() { v, _ := c.Resolve(bg(), sink.Token()); appGot <- v }()
	waitClosed(t, g.started, "creator")
	obsGot := make(chan any, 1)
	go func() { v, _ := o.r.Resolve(bg(), sink.Token()); obsGot <- v }()
	eventually(t, "the observer to join the latch", func() bool {
		return typewirego.ObserverWaiting(c, sink.Token())
	})
	g.open()
	a, b := recv(t, appGot, "app"), recv(t, obsGot, "observer")
	if !slices.Contains(pulledLabels(c), "t.Sink") {
		t.Fatal("the value the observer waited for is not in the observer set")
	}
	if a == nil || a != b {
		t.Fatal("the observer did not share the app's creation")
	}
	if calls.Load() != 1 {
		t.Fatalf("creator ran %d times", calls.Load())
	}
}

func TestP16UnboundRequiredWireFailsFirstResolve(t *testing.T) {
	sink := leafWire("t.Sink")
	app := leafWire("t.App")
	c, _ := observedWith(nil, []typewirego.AnyWire{sink})
	defer c.Teardown(bg())
	apply(t, c, app)
	_, err := c.Resolve(bg(), app.Token())
	if !errors.Is(err, typewirego.ErrWireNotRegistered) {
		t.Fatalf("first resolve with the sink unbound = %v", err)
	}
	if _, err := c.Resolve(bg(), app.Token()); !errors.Is(err, typewirego.ErrWireNotRegistered) {
		t.Fatal("the check stopped before the sink was bound")
	}
	apply(t, c, sink)
	mustResolve(t, c, app)
}

// sinkSetup builds an observer that pulls sink on its first event after ready.
func pullOnce(t *testing.T, o *recObserver, sink typewirego.AnyWire) func() {
	var once sync.Once
	pulled := make(chan struct{})
	return func() {
		once.Do(func() {
			if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
				t.Errorf("pull: %v", err)
			}
			close(pulled)
		})
		<-pulled
	}
}

func TestP17ObserverSetCleanedLastAfterFlush(t *testing.T) {
	log := &orderLog{}
	sink := resWire("t.Sink", log)
	svc := resWire("t.Svc", log)
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { log.add("flush"); return nil }
	})
	apply(t, c, sink, svc)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	mustResolve(t, c, svc)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	want := []string{"t.Svc", "flush", "t.Sink"}
	if got := log.get(); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestP17ObserverSetIncludesRuntimeImportClosure(t *testing.T) {
	log := &orderLog{}
	cfg := resWire("t.Cfg", log)
	helper := resWire("t.Helper", log)
	sink := typewirego.Of(typewirego.Opts[*res]{Token: "t.Sink",
		Imports: []typewirego.Import{{Name: "cfg", Wire: cfg}},
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (*res, error) {
			if _, err := helper.GetInstance(ctx, r); err != nil {
				return nil, err
			}
			return newRes("t.Sink", log), nil
		}})
	svc := resWire("t.Svc", log)
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { log.add("flush"); return nil }
	})
	apply(t, c, sink, helper, svc)
	mustResolve(t, c, svc)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	if got := pulledLabels(c); !slices.Equal(got, []string{"t.Cfg", "t.Sink", "t.Helper"}) {
		t.Fatalf("observer set = %v", got)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	want := []string{"t.Svc", "flush", "t.Sink", "t.Helper", "t.Cfg"}
	if got := log.get(); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestP17TeardownWithSinkCreatedAfterService(t *testing.T) {
	log := &orderLog{}
	sink := resWire("t.Sink", log)
	svc := resWire("t.Svc", log)
	var pull func()
	c, o := observedWith(func(o *recObserver) {
		o.flush = func(context.Context) error { log.add("flush"); return nil }
	}, []typewirego.AnyWire{sink})
	pull = pullOnce(t, o, sink)
	apply(t, c, svc, sink)
	mustResolve(t, c, svc)
	pull() // the sink is created after the service
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	want := []string{"t.Svc", "flush", "t.Sink"}
	if got := log.get(); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestP17HeldBackTokenExcludedFromObserverPass(t *testing.T) {
	log := &orderLog{}
	cfg := resWire("t.Cfg", log)
	sink := resWire("t.Sink", log, cfg)
	g := newGate()
	late := typewirego.Of(typewirego.Opts[*res]{Token: "t.Late",
		Imports: []typewirego.Import{{Name: "cfg", Wire: cfg}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (*res, error) {
			g.enter()
			return newRes("t.Late", log), nil
		}})
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { log.add("flush"); return nil }
	})
	apply(t, c, sink, late)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	go c.Resolve(bg(), late.Token())
	waitClosed(t, g.started, "late creator")
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.Cfg"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	if got := log.get(); !slices.Equal(got, []string{"flush", "t.Sink"}) {
		t.Fatalf("teardown cleaned %v, want [flush t.Sink]: the held-back t.Cfg stays out of the observer pass", got)
	}
	if got := pulledLabels(c); !slices.Equal(got, []string{"t.Sink"}) {
		t.Fatalf("observer set = %v", got)
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"flush", "t.Sink", "t.Late", "t.Cfg"}) {
		t.Fatalf("final order = %v", got)
	}
}

func TestP17UndeliveredEventCountsInStatsNotError(t *testing.T) {
	log := &orderLog{}
	sink := resWire("t.Sink", log)
	c, o := observed(nil)
	apply(t, c, sink)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	// The observer set is cleaned after closed: its cleanup events are
	// counted, never delivered.
	if c.Stats().PostClosedEvents < 2 {
		t.Fatalf("PostClosedEvents = %d", c.Stats().PostClosedEvents)
	}
	if o.find(typewirego.EventCleanupStart, "t.Sink") != nil {
		t.Fatal("an event after closed was delivered")
	}
}
