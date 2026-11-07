package typewirego_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/typewirego/typewirego"
)

func TestP18ContainerContextCarriesNoCallerValues(t *testing.T) {
	var seen any = "unset"
	w := typewirego.Of(typewirego.Opts[int]{Token: "t.S", Creator: func(ctx context.Context) (int, error) {
		seen = ctx.Value(ctxKey{})
		return 1, nil
	}})
	c := typewirego.NewContainer(context.WithValue(bg(), ctxKey{}, "new-container"))
	defer c.Teardown(bg())
	apply(t, c, w)
	if _, err := c.Resolve(context.WithValue(bg(), ctxKey{}, "caller"), w.Token()); err != nil {
		t.Fatal(err)
	}
	if seen != nil {
		t.Fatalf("creator context carried %v", seen)
	}
}

func TestP18CreatorAbortContextCancelledAtClosing(t *testing.T) {
	var abort context.Context
	errAtCleanup := errors.New("unset")
	w := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.S", Creator: func(ctx context.Context) (*hookCloser, error) {
		abort = ctx
		return &hookCloser{close: func() error { errAtCleanup = ctx.Err(); return nil }}, nil
	}})
	// A creation still running when Teardown begins: it ends only when its
	// abort context is cancelled.
	started := make(chan struct{})
	unblocked := make(chan struct{})
	blocked := typewirego.Of(typewirego.Opts[int]{Token: "t.Blocked", Creator: func(ctx context.Context) (int, error) {
		close(started)
		<-ctx.Done()
		close(unblocked)
		return 0, ctx.Err()
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, w, blocked)
	mustResolve(t, c, w)
	if abort.Err() != nil {
		t.Fatal("abort context ended before Teardown")
	}
	go c.Resolve(bg(), blocked.Token())
	waitClosed(t, started, "blocked creator")
	// A bound Teardown can only finish its wait in time if Closing itself
	// cancels the in-flight creator.
	ctx, cancel := context.WithTimeout(bg(), testTimeout)
	defer cancel()
	if err := c.Teardown(ctx); err != nil {
		t.Fatalf("teardown = %v, want nil: the abort context must be cancelled when Closing begins", err)
	}
	waitClosed(t, unblocked, "the creator's abort context")
	if !errors.Is(errAtCleanup, context.Canceled) {
		t.Fatalf("at cleanup the abort context was %v, want cancelled since Closing", errAtCleanup)
	}
}

// hookCloser is a value whose Close runs a hook.
type hookCloser struct{ close func() error }

func (h *hookCloser) Close() error { return h.close() }

func TestP18LifetimeContextEndsAfterOwnCleanup(t *testing.T) {
	var life context.Context
	var atOwnCleanup, atLaterCleanup error
	afterOwn := errors.New("unset")
	// Z is created first, so its cleanup runs after A's.
	z := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.Z", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error { afterOwn = life.Err(); return nil }}, nil
	}})
	a := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.A",
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (*hookCloser, error) {
			life = typewirego.Lifetime(d)
			return &hookCloser{close: func() error { atOwnCleanup = life.Err(); return nil }}, nil
		}})
	b := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.B", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error { atLaterCleanup = life.Err(); return nil }}, nil
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, z, a, b)
	mustResolve(t, c, z)
	mustResolve(t, c, a)
	mustResolve(t, c, b)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if atLaterCleanup != nil || atOwnCleanup != nil {
		t.Fatalf("Lifetime ended before its own cleanup ran: %v, %v", atLaterCleanup, atOwnCleanup)
	}
	if !errors.Is(afterOwn, context.Canceled) {
		t.Fatalf("at the next slot A's Lifetime was %v, want ended right after A's own cleanup", afterOwn)
	}
	// Pinned value: Lifetime of a zero Deps is context.Background().
	if typewirego.Lifetime(typewirego.Deps{}) != context.Background() {
		t.Fatal("Lifetime of zero Deps is not Background")
	}
}

func TestP19TeardownTimeoutWithRunningCreatorWhoseImportIsCached(t *testing.T) {
	log := &orderLog{}
	s := resWire("t.S", log)
	g := newGate()
	x := typewirego.Of(typewirego.Opts[*res]{Token: "t.X",
		Imports: []typewirego.Import{{Name: "s", Wire: s}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (*res, error) {
			g.enter()
			return newRes("t.X", log), nil
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, x)
	mustResolve(t, c, s)
	go c.Resolve(bg(), x.Token())
	waitClosed(t, g.started, "creator")
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.Running, []string{"t.X"}) || !slices.Equal(tt.HeldBack, []string{"t.S"}) {
		t.Fatalf("Running %v HeldBack %v", tt.Running, tt.HeldBack)
	}
	if len(log.get()) != 0 {
		t.Fatalf("the running creator's import was cleaned under it: %v", log.get())
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"t.X", "t.S"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestP19TimeoutStillRunsWholeTeardownPath(t *testing.T) {
	log := &orderLog{}
	other := resWire("t.Other", log)
	sink := resWire("t.Sink", log)
	g := newGate()
	stuck := typewirego.Of(typewirego.Opts[int]{Token: "t.Stuck", Creator: func(context.Context) (int, error) {
		g.enter()
		return 1, nil
	}})
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { log.add("flush"); return nil }
	})
	apply(t, c, other, sink, stuck)
	mustResolve(t, c, other)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	go c.Resolve(bg(), stuck.Token())
	waitClosed(t, g.started, "creator")
	timeoutErr(t, c.Teardown(expiredCtx()))
	if got := log.get(); !slices.Equal(got, []string{"t.Other", "flush", "t.Sink"}) {
		t.Fatalf("path = %v, want main pass, Flush, observer set", got)
	}
	closed := o.find(typewirego.EventClosed, "")
	if closed == nil || len(closed.InFlight) != 1 {
		t.Fatalf("closed event = %+v", closed)
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
}

func TestP19LateCreatorCleansOnlyItsOwnValue(t *testing.T) {
	log := &orderLog{}
	s := resWire("t.S", log)
	gx, gz := newGate(), newGate()
	x := typewirego.Of(typewirego.Opts[*res]{Token: "t.X",
		Imports: []typewirego.Import{{Name: "s", Wire: s}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (*res, error) {
			gx.enter()
			return newRes("t.X", log), nil
		}})
	z := typewirego.Of(typewirego.Opts[*res]{Token: "t.Z", Creator: func(context.Context) (*res, error) {
		gz.enter()
		return newRes("t.Z", log), nil
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, x, z)
	go c.Resolve(bg(), x.Token())
	go c.Resolve(bg(), z.Token())
	waitClosed(t, gx.started, "x")
	waitClosed(t, gz.started, "z")
	timeoutErr(t, c.Teardown(expiredCtx()))
	gx.open()
	eventually(t, "x to clean itself", func() bool { return log.count("t.X") == 1 })
	if log.count("t.S") != 0 {
		t.Fatal("a late creator cleaned its import while another late creation still runs")
	}
	gz.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got[len(got)-1:], []string{"t.S"}) || len(got) != 3 {
		t.Fatalf("order = %v, want the held-back t.S last", got)
	}
}

func TestP19HeldBackClosureCleanedOnceByLastFinisher(t *testing.T) {
	log := &orderLog{}
	s := resWire("t.S", log)
	m := resWire("t.M", log, s)
	g1, g2 := newGate(), newGate()
	x1 := gatedRes("t.X1", g1, log, m)
	x2 := gatedRes("t.X2", g2, log, m)
	c := typewirego.NewContainer(bg())
	apply(t, c, x1, x2)
	go c.Resolve(bg(), x1.Token())
	go c.Resolve(bg(), x2.Token())
	waitClosed(t, g1.started, "x1")
	waitClosed(t, g2.started, "x2")
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.S", "t.M"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	g1.open()
	eventually(t, "x1 to clean its own value", func() bool { return log.count("t.X1") == 1 })
	if log.count("t.M") != 0 || log.count("t.S") != 0 {
		t.Fatalf("held-back values cleaned before the last finisher: %v", log.get())
	}
	g2.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"t.X1", "t.X2", "t.M", "t.S"}) {
		t.Fatalf("order = %v, want each once and the closure after the last finisher", got)
	}
}

func TestP19CreationFinishingDuringWaitIsClosingFailure(t *testing.T) {
	// A singleton finishing during the wait: its caller gets ErrClosing, never
	// the value.
	g := newGate()
	s := typewirego.Of(typewirego.Opts[string]{Token: "t.S", Creator: func(ctx context.Context) (string, error) {
		g.enter()
		return "value", nil
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, s)
	type result struct {
		v   any
		err error
	}
	rc := make(chan result, 1)
	go func() { v, err := c.Resolve(bg(), s.Token()); rc <- result{v, err} }()
	waitClosed(t, g.started, "creator")
	done := make(chan error, 1)
	go func() { done <- c.Teardown(bg()) }()
	eventually(t, "closing", func() bool {
		_, err := c.GetCached(s.Token())
		return errors.Is(err, typewirego.ErrClosing)
	})
	g.open()
	r := recv(t, rc, "resolve")
	if r.v != nil || !errors.Is(r.err, typewirego.ErrClosing) {
		t.Fatalf("caller got %v, %v; want ErrClosing", r.v, r.err)
	}
	if err := recv(t, done, "teardown"); err != nil {
		t.Fatal(err)
	}
}

func TestP20FlushBudgetIsQuarterOfRemainingTime(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(bg(), now.Add(4*time.Second))
	defer cancel()
	if got := typewirego.FlushBudgetFor(ctx, 5*time.Second, now); got != time.Second {
		t.Fatalf("budget = %v, want 1s", got)
	}
	var left time.Duration
	c, _ := observed(func(o *recObserver) {
		o.flush = func(fctx context.Context) error {
			dl, _ := fctx.Deadline()
			left = time.Until(dl)
			return nil
		}
	}, typewirego.WithFlushBudget(time.Hour))
	tctx, cancel2 := context.WithTimeout(bg(), 400*time.Millisecond)
	defer cancel2()
	if err := c.Teardown(tctx); err != nil {
		t.Fatal(err)
	}
	if left <= 50*time.Millisecond || left > 100*time.Millisecond {
		t.Fatalf("Flush allowance left %v, want about a quarter of 400ms", left)
	}
}

func TestP20FlushBudgetUsesConfiguredValueWithoutDeadline(t *testing.T) {
	if got := typewirego.FlushBudgetFor(bg(), 3*time.Second, time.Now()); got != 3*time.Second {
		t.Fatalf("budget = %v", got)
	}
	var left time.Duration
	c, _ := observed(func(o *recObserver) {
		o.flush = func(fctx context.Context) error {
			dl, ok := fctx.Deadline()
			if !ok {
				t.Error("Flush context has no deadline")
			}
			left = time.Until(dl)
			return nil
		}
	}, typewirego.WithFlushBudget(2*time.Second))
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if left <= time.Second || left > 2*time.Second {
		t.Fatalf("Flush allowance left %v, want about the configured 2s", left)
	}
}

func TestP20FlushBudgetSurvivesWaitTimeout(t *testing.T) {
	g := newGate()
	stuck := typewirego.Of(typewirego.Opts[int]{Token: "t.Stuck", Creator: func(context.Context) (int, error) {
		g.enter()
		return 1, nil
	}})
	var flushErrAtStart error = errors.New("unset")
	var hasDeadline bool
	c, _ := observed(func(o *recObserver) {
		o.flush = func(fctx context.Context) error {
			flushErrAtStart = fctx.Err()
			_, hasDeadline = fctx.Deadline()
			return nil
		}
	}, typewirego.WithFlushBudget(time.Hour))
	apply(t, c, stuck)
	go c.Resolve(bg(), stuck.Token())
	waitClosed(t, g.started, "creator")
	tctx, cancel := context.WithTimeout(bg(), 200*time.Millisecond)
	defer cancel()
	timeoutErr(t, c.Teardown(tctx))
	if tctx.Err() == nil {
		t.Fatal("teardown context should have expired during the wait")
	}
	if flushErrAtStart != nil || !hasDeadline {
		t.Fatalf("Flush context = %v (deadline %v), want a live context with its own timeout", flushErrAtStart, hasDeadline)
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
}

// shutdowner records the context its Shutdown receives.
type shutdowner struct {
	fn func(ctx context.Context) error
}

func (s *shutdowner) Shutdown(ctx context.Context) error { return s.fn(ctx) }

func TestP20ObserverSetCleanupGetsSecondAllowance(t *testing.T) {
	const budget = 100 * time.Millisecond
	var left time.Duration
	var errAtStart error = errors.New("unset")
	sink := typewirego.Of(typewirego.Opts[*shutdowner]{Token: "t.Sink", Creator: func(context.Context) (*shutdowner, error) {
		return &shutdowner{fn: func(ctx context.Context) error {
			errAtStart = ctx.Err()
			dl, _ := ctx.Deadline()
			left = time.Until(dl)
			return nil
		}}, nil
	}})
	c, o := observed(func(o *recObserver) {
		// Flush uses up its whole allowance.
		o.flush = func(fctx context.Context) error { <-fctx.Done(); return fctx.Err() }
	}, typewirego.WithFlushBudget(budget))
	apply(t, c, sink)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if errAtStart != nil || left <= budget/2 {
		t.Fatalf("observer-set cleanup got %v with %v left, want a fresh allowance", errAtStart, left)
	}
	if c.Stats().FlushErrors != 1 {
		t.Fatalf("FlushErrors = %d", c.Stats().FlushErrors)
	}
}

// gatedRes is a singleton returning a fresh *res once g opens.
func gatedRes(label string, g *gate, log *orderLog, imports ...typewirego.AnyWire) typewirego.TypeWire[*res] {
	var imps []typewirego.Import
	for _, w := range imports {
		imps = append(imps, typewirego.Import{Name: w.Token().Label(), Wire: w})
	}
	return typewirego.Of(typewirego.Opts[*res]{Token: label, Imports: imps,
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (*res, error) {
			g.enter()
			return newRes(label, log), nil
		}})
}
