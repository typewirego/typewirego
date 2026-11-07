package typewirego_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/typewirego/typewirego"
)

func TestANilInterfaceValueResolvesToTheZeroValue(t *testing.T) {
	var calls atomic.Int32
	w := typewirego.Of(typewirego.Opts[io.Reader]{Token: "t.Reader", Creator: func(context.Context) (io.Reader, error) {
		calls.Add(1)
		return nil, nil
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	for range 2 {
		r, err := w.GetInstance(bg(), c)
		if err != nil || r != nil {
			t.Fatalf("GetInstance = %v, %v; want nil, nil", r, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("a nil value was not cached: %d calls", calls.Load())
	}
	if v, err := c.GetCached(w.Token()); err != nil || v != nil {
		t.Fatalf("GetCached = %v, %v", v, err)
	}
}

func TestCreationBelowAnEndedCreatorResolvesItWithoutACycle(t *testing.T) {
	yGate := newGate()
	yGot := make(chan error, 1)
	var x typewirego.TypeWire[string]
	y := typewirego.Of(typewirego.Opts[string]{Token: "t.Y",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			yGate.enter()
			// Y was started by X's creator, which has returned since: X is
			// no longer on Y's chain.
			_, err := r.Resolve(ctx, x.Token())
			yGot <- err
			return "y", err
		}})
	x = typewirego.Of(typewirego.Opts[string]{Token: "t.X",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			wctx, cancel := context.WithTimeout(ctx, time.Millisecond)
			defer cancel()
			_, _ = r.Resolve(wctx, y.Token()) // gives up waiting; Y keeps running
			return "x", nil
		}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, x, y)
	mustResolve(t, c, x)
	yGate.open()
	if err := recv(t, yGot, "y's resolve of x"); err != nil {
		t.Fatalf("resolve below an ended creator = %v, want the cached value", err)
	}
	mustResolve(t, c, y)
}

func TestCreatorGoexitReleasesLatch(t *testing.T) {
	var calls atomic.Int32
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.Goexit", Creator: func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			runtime.Goexit()
		}
		return "ok", nil
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	if _, err := c.Resolve(bg(), w.Token()); !errors.Is(err, typewirego.ErrCreator) {
		t.Fatalf("Goexit surfaced as %v, want a creator error", err)
	}
	if v := mustResolve(t, c, w); v != "ok" {
		t.Fatalf("retry = %v", v)
	}
}

func TestCreatorResolverOutlivesItsCreatorWithoutAFalseCycle(t *testing.T) {
	var kept typewirego.Resolver
	other := leafWire("t.Other")
	x := typewirego.Of(typewirego.Opts[string]{Token: "t.X",
		CreateWith: func(_ context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			kept = r
			return "x", nil
		}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, x, other)
	mustResolve(t, c, x)
	if v, err := kept.Resolve(bg(), x.Token()); err != nil || v != "x" {
		t.Fatalf("kept resolver resolving its own token = %v, %v", v, err)
	}
	if v, err := kept.Resolve(bg(), other.Token()); err != nil || v != "t.Other" {
		t.Fatalf("kept resolver = %v, %v", v, err)
	}
}

func TestCreatorResolvingItsOwnTokenWhileRunningIsACycle(t *testing.T) {
	var x typewirego.TypeWire[string]
	x = typewirego.Of(typewirego.Opts[string]{Token: "t.X",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			errc := make(chan error, 1)
			// Another goroutine, same chain: the creator has not returned.
			go func() { _, err := r.Resolve(ctx, x.Token()); errc <- err }()
			return "x", <-errc
		}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, x)
	_, err := c.Resolve(ctxWithTimeout(t), x.Token())
	var cycle *typewirego.CircularDependencyError
	if !errors.As(err, &cycle) || !slices.Equal(cycle.Path, []string{"t.X", "t.X"}) {
		t.Fatalf("got %v, want a cycle t.X -> t.X", err)
	}
}

func TestDepRejectsAWireOutsideTheImportsWithoutAContainer(t *testing.T) {
	if _, err := typewirego.Dep(typewirego.Deps{}, leafWire("t.X")); !errors.Is(err, typewirego.ErrNotImported) {
		t.Fatal(err)
	}
}

func TestGetCachedNeverRunsACreator(t *testing.T) {
	var calls atomic.Int32
	s := typewirego.Of(typewirego.Opts[string]{Token: "t.S", Creator: func(context.Context) (string, error) {
		calls.Add(1)
		return "s", nil
	}})
	tr := typewirego.Of(typewirego.Opts[string]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (string, error) { calls.Add(1); return "t", nil }})
	c := typewirego.NewContainer(bg())
	apply(t, c, s, tr)
	if _, err := c.GetCached(s.Token()); !errors.Is(err, typewirego.ErrNotResolved) {
		t.Fatalf("uncreated = %v", err)
	}
	if _, err := c.GetCached(tr.Token()); !errors.Is(err, typewirego.ErrNotResolved) {
		t.Fatalf("transient = %v", err)
	}
	if _, err := c.GetCached(leafWire("t.Unbound").Token()); !errors.Is(err, typewirego.ErrWireNotRegistered) {
		t.Fatalf("unbound = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("GetCached ran a creator")
	}
	mustResolve(t, c, s)
	if v, err := c.GetCached(s.Token()); err != nil || v != "s" {
		t.Fatalf("cached = %v, %v", v, err)
	}
	_ = c.Teardown(bg())
	if _, err := c.GetCached(s.Token()); !errors.Is(err, typewirego.ErrClosing) {
		t.Fatalf("after teardown = %v", err)
	}
}

func TestThreeGoroutineWaitForCycleIsCircularDependencyError(t *testing.T) {
	started := make(chan struct{}, 3)
	all := make(chan struct{})
	ws := make([]typewirego.TypeWire[string], 3)
	for i, label := range []string{"t.A", "t.B", "t.C"} {
		next := (i + 1) % 3
		ws[i] = typewirego.Of(typewirego.Opts[string]{Token: label,
			CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
				started <- struct{}{}
				<-all
				_, err := r.Resolve(ctx, ws[next].Token())
				return label, err
			}})
	}
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, ws[0], ws[1], ws[2])
	ctx := ctxWithTimeout(t)
	errs := make(chan error, 3)
	for _, w := range ws {
		go func() { _, err := c.Resolve(ctx, w.Token()); errs <- err }()
	}
	for range 3 {
		recv(t, started, "creator start")
	}
	close(all)
	for range 3 {
		err := recv(t, errs, "root resolve")
		var cycle *typewirego.CircularDependencyError
		if !errors.As(err, &cycle) || len(cycle.Path) != 4 || cycle.Path[0] != cycle.Path[3] {
			t.Fatalf("got %v, want a three-token cycle", err)
		}
	}
}

func TestWaitBegunByAnEndedCreatorsGoroutineIsNoCycle(t *testing.T) {
	yGate := newGate()
	var x typewirego.TypeWire[string]
	y := typewirego.Of(typewirego.Opts[string]{Token: "t.Y",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			yGate.enter()
			_, err := r.Resolve(ctx, x.Token())
			return "y", err
		}})
	xReturned := make(chan struct{})
	later := make(chan error, 1)
	x = typewirego.Of(typewirego.Opts[string]{Token: "t.X",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			// A goroutine X started keeps X's resolver and waits on Y only
			// after X has returned: X waits on nothing, so Y resolving X is
			// no cycle.
			go func() {
				<-xReturned
				_, err := r.Resolve(bg(), y.Token())
				later <- err
			}()
			return "x", nil
		}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, x, y)
	mustResolve(t, c, x)
	close(xReturned)
	waitClosed(t, yGate.started, "y")
	eventually(t, "the late wait on y", func() bool { return typewirego.LatchWaiters(c, y.Token()) == 1 })
	yGate.open()
	if err := recv(t, later, "the late wait"); err != nil {
		t.Fatalf("wait from an ended creator's goroutine = %v", err)
	}
}

func TestWaitForCycleThroughATransientFrame(t *testing.T) {
	xIn, yIn := make(chan struct{}), make(chan struct{})
	var x, y typewirego.TypeWire[string]
	tr := typewirego.Of(typewirego.Opts[string]{Token: "t.T", Scope: typewirego.Transient,
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			_, err := r.Resolve(ctx, y.Token())
			return "t", err
		}})
	x = typewirego.Of(typewirego.Opts[string]{Token: "t.X",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			close(xIn)
			<-yIn
			_, err := r.Resolve(ctx, tr.Token()) // inline, on X's goroutine
			return "x", err
		}})
	y = typewirego.Of(typewirego.Opts[string]{Token: "t.Y",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			close(yIn)
			<-xIn
			_, err := r.Resolve(ctx, x.Token())
			return "y", err
		}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, x, y, tr)
	ctx := ctxWithTimeout(t)
	errs := make(chan error, 2)
	go func() { _, err := c.Resolve(ctx, x.Token()); errs <- err }()
	go func() { _, err := c.Resolve(ctx, y.Token()); errs <- err }()
	for range 2 {
		if err := recv(t, errs, "root"); !errors.Is(err, typewirego.ErrCircularDependency) {
			t.Fatalf("got %v, want a cycle", err)
		}
	}
}

func TestWithCreatorKeepsKeepOpen(t *testing.T) {
	log := &orderLog{}
	borrowed := typewirego.Of(typewirego.Opts[*res]{Token: "t.Borrowed", KeepOpen: true,
		Creator: func(context.Context) (*res, error) { return newRes("orig", log), nil }})
	fake := borrowed.WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[*res]) (*res, error) {
		return newRes("fake", log), nil
	})
	c := typewirego.NewContainer(bg())
	apply(t, c, fake)
	mustResolve(t, c, fake)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if len(log.get()) != 0 {
		t.Fatalf("an override of a KeepOpen wire was closed: %v", log.get())
	}
}

func TestWithCreatorResolvesDeclaredImportsOnlyWhenOriginalRuns(t *testing.T) {
	var depCalls atomic.Int32
	dep := typewirego.Of(typewirego.Opts[string]{Token: "t.Dep", Creator: func(context.Context) (string, error) {
		depCalls.Add(1)
		return "real-dep", nil
	}})
	svc := typewirego.Of(typewirego.Opts[string]{Token: "t.Svc",
		Imports: []typewirego.Import{{Name: "dep", Wire: dep}},
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (string, error) {
			v, err := typewirego.Dep(d, dep)
			return "svc(" + v + ")", err
		}})
	for _, callOriginal := range []bool{false, true} {
		depCalls.Store(0)
		var sawImport error
		override := svc.WithCreator(func(ctx context.Context, d typewirego.Deps, r typewirego.Resolver, original typewirego.Creator[string]) (string, error) {
			_, sawImport = typewirego.Dep(d, dep)
			if callOriginal {
				v, err := original(ctx, d, r)
				return "wrapped " + v, err
			}
			return "fake", nil
		})
		c := typewirego.NewContainer(bg())
		apply(t, c, override)
		v := mustResolve(t, c, override)
		if !errors.Is(sawImport, typewirego.ErrNotImported) {
			t.Fatal("the override's Deps carried a resolved import")
		}
		want, wantCalls := "fake", int32(0)
		if callOriginal {
			want, wantCalls = "wrapped svc(real-dep)", 1
		}
		if v != want || depCalls.Load() != wantCalls {
			t.Fatalf("callOriginal=%v: value %v, dep calls %d", callOriginal, v, depCalls.Load())
		}
		if !c.IsBound(dep.Token()) {
			t.Fatal("the override's Apply did not bind the declared import")
		}
		_ = c.Teardown(bg())
	}
}

func TestWithObserverTwicePanics(t *testing.T) {
	f := func(typewirego.Resolver) typewirego.Observer { return &recObserver{} }
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("a second WithObserver did not panic")
		}
	}()
	typewirego.NewContainer(bg(), typewirego.WithObserver(f), typewirego.WithObserver(f))
}

type innerErr struct{}

func (innerErr) Error() string { return "inner" }

func TestErrClassMatchesSentinelsBeforeTheInnermostType(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, "canceled"},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{&typewirego.CreatorError{Label: "a", Cause: &typewirego.CircularDependencyError{}}, "cycle"},
		{&typewirego.CreatorError{Label: "a", Cause: context.Canceled}, "canceled"},
		{fmt.Errorf("%w: x", typewirego.ErrWireNotRegistered), "not_registered"},
		{typewirego.ErrClosing, "closing"},
		{&typewirego.TeardownTimeoutError{}, "teardown_timeout"},
		{typewirego.ErrNotResolved, "not_resolved"},
		{typewirego.ErrNotImported, "not_imported"},
		{typewirego.ErrObserverResolve, "observer_resolve"},
		{&typewirego.CreatorError{Label: "a", Cause: errors.New("plain")}, "creator"},
		{fmt.Errorf("wrap: %w", innerErr{}), "typewirego_test.innerErr"},
		{errors.Join(innerErr{}, errors.New("second")), "typewirego_test.innerErr"},
		{errors.New("text never matters"), "*errors.errorString"},
	}
	for _, tc := range cases {
		if got := typewirego.ErrClassOf(tc.err); got != tc.want {
			t.Errorf("ErrClassOf(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestFlushBudgetArithmetic(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) context.Context {
		ctx, cancel := context.WithDeadline(bg(), now.Add(d))
		t.Cleanup(cancel)
		return ctx
	}
	cases := []struct {
		ctx        context.Context
		configured time.Duration
		want       time.Duration
	}{
		{bg(), 5 * time.Second, 5 * time.Second},
		{at(4 * time.Second), 5 * time.Second, time.Second},
		{at(40 * time.Second), 5 * time.Second, 5 * time.Second},
		{at(-time.Second), 5 * time.Second, 0},
		{at(4 * time.Second), 0, 0},
	}
	for _, tc := range cases {
		if got := typewirego.FlushBudgetFor(tc.ctx, tc.configured, now); got != tc.want {
			t.Errorf("FlushBudgetFor(%v) = %v, want %v", tc.configured, got, tc.want)
		}
	}
	// A negative configured budget is zero: Flush runs with an allowance
	// that is already over.
	left := time.Hour
	hasDeadline := false
	c, o := observed(func(o *recObserver) {
		o.flush = func(ctx context.Context) error {
			var dl time.Time
			dl, hasDeadline = ctx.Deadline()
			left = time.Until(dl)
			return nil
		}
	}, typewirego.WithFlushBudget(-time.Hour))
	_ = c.Teardown(bg())
	if o.flushed.Load() != 1 || c.Stats().FlushErrors != 0 || !hasDeadline {
		t.Fatalf("flushed %d, FlushErrors %d, deadline %v", o.flushed.Load(), c.Stats().FlushErrors, hasDeadline)
	}
	if left > 0 || left < -typewirego.CleanupGrace-time.Second {
		t.Fatalf("negative budget left %v for Flush, want a zero allowance", left)
	}
}

func TestNewContainerContextLeavesCreatorsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(bg())
	g := newGate()
	var creatorErr error
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.W", Creator: func(cctx context.Context) (string, error) {
		g.enter()
		creatorErr = cctx.Err()
		return "w", nil
	}})
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(bg())
	apply(t, c, w)
	done := make(chan any, 1)
	go func() { v, _ := c.Resolve(bg(), w.Token()); done <- v }()
	waitClosed(t, g.started, "creator")
	cancel() // a graceful drain begins
	g.open()
	if v := recv(t, done, "resolve"); v != "w" || creatorErr != nil {
		t.Fatalf("value %v, creator ctx err %v", v, creatorErr)
	}
}

func TestEventsCarryNodeIDsDurationsAndCloser(t *testing.T) {
	log := &orderLog{}
	a := resWire("t.A", log)
	fails := typewirego.Of(typewirego.Opts[int]{Token: "t.Fails",
		Creator: func(context.Context) (int, error) { return 0, errors.New("no") }})
	c, o := observed(nil)
	apply(t, c, a, fails)
	mustResolve(t, c, a)
	_, _ = c.Resolve(bg(), fails.Token())
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	reg := o.find(typewirego.EventRegistered, "t.A")
	ok := o.find(typewirego.EventResolveOK, "t.A")
	failed := o.find(typewirego.EventResolveFailed, "t.Fails")
	cleaned := o.find(typewirego.EventCleanupOK, "t.A")
	if reg == nil || ok == nil || failed == nil || cleaned == nil {
		t.Fatalf("missing events: %v", o.kinds())
	}
	if reg.NodeID != "n1" || ok.NodeID != "n1" || cleaned.NodeID != "n1" || failed.NodeID != "n2" {
		t.Fatalf("node ids: %s %s %s %s", reg.NodeID, ok.NodeID, cleaned.NodeID, failed.NodeID)
	}
	if reg.Scope != typewirego.Singleton {
		t.Fatalf("registered scope %q", reg.Scope)
	}
	if ok.Closer != "close-err" || cleaned.Closer != "close-err" {
		t.Fatalf("closer %q / %q", ok.Closer, cleaned.Closer)
	}
	if ok.Duration <= 0 || failed.Duration <= 0 || cleaned.Duration <= 0 {
		t.Fatalf("durations %v %v %v", ok.Duration, failed.Duration, cleaned.Duration)
	}
	if failed.ErrClass != "creator" || !slices.Equal(ok.Path, []string{"t.A"}) {
		t.Fatalf("errclass %q path %v", failed.ErrClass, ok.Path)
	}
	for _, e := range o.got() {
		if strings.Contains(fmt.Sprint(e), "0x") {
			t.Fatalf("an event carries a value: %+v", e)
		}
	}
}
