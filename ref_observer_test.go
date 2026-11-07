package typewirego_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/typewirego/typewirego"
)

func TestCancelledObserverWaitLeavesNoObserverSetMember(t *testing.T) {
	g := newGate()
	s := gated("t.S", typewirego.Singleton, g, 1, false)
	c, o := observed(nil)
	defer c.Teardown(bg())
	apply(t, c, s)
	go c.Resolve(bg(), s.Token())
	waitClosed(t, g.started, "creator")
	ctx, cancel := context.WithCancel(bg())
	errc := make(chan error, 1)
	go func() { _, err := o.r.Resolve(ctx, s.Token()); errc <- err }()
	eventually(t, "the observer's wait", func() bool { return typewirego.ObserverWaiting(c, s.Token()) })
	cancel()
	if err := recv(t, errc, "observer"); !errors.Is(err, context.Canceled) {
		t.Fatalf("observer wait = %v", err)
	}
	g.open()
	eventually(t, "s cached", func() bool { _, err := c.GetCached(s.Token()); return err == nil })
	if got := pulledLabels(c); len(got) != 0 {
		t.Fatalf("a wait that ended without a value left %v in the observer set", got)
	}
}

func TestDrainerBetweenEventsAtTheDeadlineIsStoppedAndFlushRuns(t *testing.T) {
	release := make(chan struct{})
	var paused atomic.Bool
	c, o := observed(nil, typewirego.WithFlushBudget(0))
	o.mu.Lock()
	o.onEvent = func(e typewirego.Event) {
		if e.Kind == typewirego.EventClosing {
			paused.Store(true)
		}
	}
	o.mu.Unlock()
	typewirego.SetBetweenEventsHook(c, func() {
		if paused.Load() {
			<-release // stuck between deliveries, not inside OnEvent
		}
	})
	w := resWire("t.W", &orderLog{})
	apply(t, c, w)
	mustResolve(t, c, w)
	err := c.Teardown(bg())
	close(release)
	if err != nil {
		t.Fatalf("teardown = %v: a drainer between events is stopped, not abandoned", err)
	}
	if o.flushed.Load() != 1 || c.Stats().FlushErrors != 0 {
		t.Fatalf("Flush ran %d times with %d errors, want once", o.flushed.Load(), c.Stats().FlushErrors)
	}
	if c.Stats().QueueDrops == 0 {
		t.Fatal("the stopped drainer's backlog was not counted")
	}
	waitClosed(t, typewirego.DrainerExited(c), "the drainer to exit")
	events := o.got()
	if events[len(events)-1].Kind != typewirego.EventClosing || o.overlap.Load() {
		t.Fatalf("the stopped drainer delivered more events: %v", o.kinds())
	}
}

func TestFlushIgnoringItsContextReturnsAtTheAllowance(t *testing.T) {
	const allowance = 10 * time.Millisecond
	release := make(chan struct{})
	c, _ := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { <-release; return errors.New("ignored") }
	}, typewirego.WithFlushBudget(allowance))
	start := time.Now()
	if err := c.Teardown(bg()); err != nil {
		t.Fatalf("teardown = %v (no observer set, so nothing is held back)", err)
	}
	if el := time.Since(start); el > allowance+typewirego.CleanupGrace+400*time.Millisecond {
		t.Fatalf("Teardown waited %v for a Flush ignoring its context, want about %v", el, allowance+typewirego.CleanupGrace)
	}
	if c.Stats().FlushErrors != 1 {
		t.Fatalf("FlushErrors = %d", c.Stats().FlushErrors)
	}
	short, cancel := context.WithTimeout(bg(), 10*time.Millisecond)
	defer cancel()
	if err := c.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait returned %v before the overrunning Flush did", err)
	}
	close(release)
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatalf("Wait = %v, want nil: Flush results are never reported", err)
	}
	if c.Stats().FlushErrors != 1 {
		t.Fatal("the late Flush result was counted twice")
	}
}

func TestGraphDOTMarksObserverPulledNodes(t *testing.T) {
	sink := leafWire("t.Sink")
	app := leafWire("t.App")
	c, o := observed(nil)
	defer c.Teardown(bg())
	apply(t, c, sink, app)
	mustResolve(t, c, app)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	dot := c.Graph().DOT()
	if !strings.Contains(dot, "  n1 [label=\"t.Sink\", observer_pulled=true]\n") ||
		!strings.Contains(dot, "  n2 [label=\"t.App\"]\n") {
		t.Fatalf("DOT:\n%s", dot)
	}
	b, _ := c.Graph().JSON()
	if !strings.Contains(string(b), `"label":"t.Sink","scope":"singleton","created":true,"overridden":false,"observer_pulled":true`) {
		t.Fatalf("JSON: %s", b)
	}
}

func TestGraphObserverSetExcludesTransientsAndHeldBackValues(t *testing.T) {
	log := &orderLog{}
	cfg := resWire("t.Cfg", log)
	tr := typewirego.Of(typewirego.Opts[int]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (int, error) { return 1, nil }})
	sink := resWire("t.Sink", log, cfg, tr)
	g := newGate()
	late := gated("t.Late", typewirego.Singleton, g, 1, false, cfg)
	c, o := observed(nil)
	apply(t, c, sink, late)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	if got := pulledLabels(c); !slices.Equal(got, []string{"t.Cfg", "t.Sink"}) {
		t.Fatalf("before teardown: %v (the transient is never a member)", got)
	}
	startLate(t, c, late, g)
	timeoutErr(t, c.Teardown(expiredCtx()))
	if got := pulledLabels(c); !slices.Equal(got, []string{"t.Sink"}) {
		t.Fatalf("after teardown: %v (the held-back t.Cfg is not a member)", got)
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
}

func TestObserverFactoryPanicDisablesTheObserver(t *testing.T) {
	w := leafWire("t.W")
	c := typewirego.NewContainer(bg(), typewirego.WithObserver(func(typewirego.Resolver) typewirego.Observer {
		panic("factory bug")
	}))
	if c.Stats().ObserverPanics != 1 {
		t.Fatalf("ObserverPanics = %d", c.Stats().ObserverPanics)
	}
	apply(t, c, w)
	mustResolve(t, c, w)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
}

func TestObserverPullRacingTeardownNeverLeavesTheSinkInTheMainPass(t *testing.T) {
	for range 50 {
		log := &orderLog{}
		sink := resWire("t.Sink", log)
		c, o := observed(func(o *recObserver) {
			o.flush = func(context.Context) error { log.add("flush"); return nil }
		})
		apply(t, c, sink)
		mustResolve(t, c, sink)
		var pulled atomic.Bool
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := o.r.Resolve(bg(), sink.Token()); err == nil {
				pulled.Store(true)
			}
		})
		if err := c.Teardown(bg()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		got := log.get()
		if pulled.Load() && slices.Index(got, "t.Sink") < slices.Index(got, "flush") {
			t.Fatalf("the observer got the sink, which was then cleaned before Flush: %v", got)
		}
		if log.count("t.Sink") != 1 {
			t.Fatalf("sink cleaned %d times", log.count("t.Sink"))
		}
	}
}

func TestObserverPullWakingAfterTheTeardownSnapshotIsInTheObserverSet(t *testing.T) {
	log := &orderLog{}
	g := newGate()
	sink := gated("t.Sink", typewirego.Singleton, g, newRes("t.Sink", log), false)
	obsResult := make(chan error, 1)
	resume := make(chan struct{})
	// The main pass runs after the snapshot: only then does the observer's
	// waiter get to wake.
	releaser := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.Releaser", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error {
			close(resume)
			if err := recv(t, obsResult, "observer pull"); err != nil {
				t.Errorf("observer pull woken after the snapshot = %v, want the value", err)
			}
			log.add("t.Releaser")
			return nil
		}}, nil
	}})
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { log.add("flush"); return nil }
	})
	apply(t, c, sink, releaser)
	mustResolve(t, c, releaser)
	typewirego.SetAfterWakeHook(c, func(tok typewirego.Token) {
		if tok == sink.Token() {
			<-resume
		}
	})
	go c.Resolve(bg(), sink.Token())
	waitClosed(t, g.started, "sink")
	go func() { _, err := o.r.Resolve(bg(), sink.Token()); obsResult <- err }()
	eventually(t, "the observer's wait", func() bool { return typewirego.ObserverWaiting(c, sink.Token()) })
	g.open()
	eventually(t, "sink cached", func() bool { _, err := c.GetCached(sink.Token()); return err == nil })
	if !typewirego.ObserverWaiting(c, sink.Token()) {
		t.Fatal("the observer woke before the snapshot")
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"t.Releaser", "flush", "t.Sink"}) {
		t.Fatalf("order = %v, want the waited-on sink cleaned with the observer set", got)
	}
}

func TestObserverSetIsFrozenAtTheTeardownSnapshot(t *testing.T) {
	log := &orderLog{}
	sink := resWire("t.Sink", log)
	other := resWire("t.Other", log)
	inPass := make(chan struct{})
	release := make(chan struct{})
	blocker := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.Blocker", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error { close(inPass); <-release; return nil }}, nil
	}})
	c, o := observed(nil)
	apply(t, c, sink, other, blocker)
	mustResolve(t, c, sink)
	mustResolve(t, c, other)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	mustResolve(t, c, blocker)
	done := make(chan error, 1)
	go func() { done <- c.Teardown(bg()) }()
	waitClosed(t, inPass, "main pass")
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatalf("member after the snapshot = %v", err)
	}
	if _, err := o.r.Resolve(bg(), other.Token()); !errors.Is(err, typewirego.ErrObserverResolve) {
		t.Fatalf("non-member after the snapshot = %v, want refused", err)
	}
	close(release)
	if err := recv(t, done, "teardown"); err != nil {
		t.Fatal(err)
	}
	if got := pulledLabels(c); !slices.Equal(got, []string{"t.Sink"}) {
		t.Fatalf("observer set = %v", got)
	}
}

func TestOverrunningFlushHoldsBackTheObserverSet(t *testing.T) {
	log := &orderLog{}
	sink := resWire("t.Sink", log)
	release := make(chan struct{})
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { <-release; log.add("flush"); return nil }
	}, typewirego.WithFlushBudget(5*time.Millisecond))
	apply(t, c, sink)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	tt := timeoutErr(t, c.Teardown(bg()))
	if !slices.Equal(tt.HeldBack, []string{"t.Sink"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	if len(log.get()) != 0 {
		t.Fatal("the observer set was cleaned while Flush may still use it")
	}
	close(release)
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"flush", "t.Sink"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestTeardownAbandonsADrainerStuckInOnEvent(t *testing.T) {
	log := &orderLog{}
	sink := resWire("t.Sink", log)
	release := make(chan struct{})
	c, o := observed(nil, typewirego.WithFlushBudget(5*time.Millisecond))
	apply(t, c, sink)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the backlog", func() bool { return o.find(typewirego.EventResolveOK, "t.Sink") != nil })
	o.mu.Lock()
	o.onEvent = func(e typewirego.Event) {
		if e.Kind == typewirego.EventClosing {
			<-release
		}
	}
	o.mu.Unlock()
	tt := timeoutErr(t, c.Teardown(bg()))
	if !slices.Equal(tt.HeldBack, []string{"t.Sink"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	s := c.Stats()
	if o.flushed.Load() != 0 || s.FlushErrors != 1 || s.QueueDrops == 0 {
		t.Fatalf("flushed %d, stats %+v", o.flushed.Load(), s)
	}
	if len(log.get()) != 0 {
		t.Fatal("the observer set was cleaned under an abandoned OnEvent")
	}
	close(release)
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("t.Sink") != 1 {
		t.Fatal("observer set not cleaned once")
	}
	if n := len(o.got()); o.got()[n-1].Kind != typewirego.EventClosing {
		t.Fatal("the abandoned drainer delivered another event")
	}
}

func TestZeroFlushBudgetNeverAbandonsADisabledDrainerWithEventsQueued(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c, o := observed(func(o *recObserver) {
		o.onEvent = func(typewirego.Event) {
			once.Do(func() { close(blocked); <-release })
			panic("disable me")
		}
	}, typewirego.WithFlushBudget(0))
	ws := []typewirego.AnyWire{leafWire("t.A"), leafWire("t.B"), leafWire("t.C")}
	apply(t, c, ws...)
	waitClosed(t, blocked, "the first OnEvent")
	// Events queue up behind the stuck first OnEvent.
	for _, w := range ws {
		mustResolve(t, c, w)
	}
	close(release)
	eventually(t, "the observer to be disabled", func() bool { return c.Stats().ObserverPanics == 1 })
	if err := c.Teardown(bg()); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	if s := c.Stats(); s.FlushErrors != 0 || s.ObserverPanics != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if o.flushed.Load() != 0 {
		t.Fatal("a disabled observer was flushed")
	}
	if n := len(o.got()); n != 1 {
		t.Fatalf("a disabled observer received %d events", n)
	}
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
}

func TestZeroFlushBudgetNeverAbandonsAnIdleDrainer(t *testing.T) {
	sink := leafWire("t.Sink")
	c, o := observed(nil, typewirego.WithFlushBudget(0))
	apply(t, c, sink)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the drainer to go idle", func() bool { return o.find(typewirego.EventResolveOK, "t.Sink") != nil })
	if err := c.Teardown(bg()); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	if o.flushed.Load() != 1 || c.Stats().FlushErrors != 0 {
		t.Fatalf("flushed %d, FlushErrors %d", o.flushed.Load(), c.Stats().FlushErrors)
	}
}
