package typewirego_test

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/typewirego/typewirego"
)

// shared returns a singleton wire that hands out v.
func shared[T any](label string, v T, keepOpen bool, imports ...typewirego.AnyWire) typewirego.TypeWire[T] {
	var imps []typewirego.Import
	for _, w := range imports {
		imps = append(imps, typewirego.Import{Name: w.Token().Label(), Wire: w})
	}
	return typewirego.Of(typewirego.Opts[T]{Token: label, Imports: imps, KeepOpen: keepOpen,
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (T, error) { return v, nil }})
}

// gated returns a wire of scope that waits on g, then returns v.
func gated[T any](label string, scope typewirego.Scope, g *gate, v T, keepOpen bool, imports ...typewirego.AnyWire) typewirego.TypeWire[T] {
	var imps []typewirego.Import
	for _, w := range imports {
		imps = append(imps, typewirego.Import{Name: w.Token().Label(), Wire: w})
	}
	return typewirego.Of(typewirego.Opts[T]{Token: label, Scope: scope, Imports: imps, KeepOpen: keepOpen,
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (T, error) {
			g.enter()
			return v, nil
		}})
}

// startLate resolves w on its own goroutine and waits until its creator runs.
func startLate(t *testing.T, c *typewirego.Container, w typewirego.AnyWire, g *gate) <-chan error {
	t.Helper()
	errc := make(chan error, 1)
	go func() { _, err := c.Resolve(bg(), w.Token()); errc <- err }()
	waitClosed(t, g.started, w.Token().Label())
	return errc
}

func TestAttachedImportIsCleanedBeforeTheValueItWasAttachedTo(t *testing.T) {
	log := &orderLog{}
	v := newRes("value", log)
	base := shared("t.Base", v, false)
	extra := resWire("t.Extra", log)
	// The anti-pattern: attach an import to an existing value and return it.
	attach := typewirego.Of(typewirego.Opts[*res]{Token: "t.Attach",
		Imports: []typewirego.Import{{Name: "base", Wire: base}, {Name: "extra", Wire: extra}},
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (*res, error) {
			return typewirego.Dep(d, base)
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, attach)
	mustResolve(t, c, attach)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	// The value keeps its first creator's slot, so the attached import goes
	// first: wrap the value in a new type instead.
	if got := log.get(); !slices.Equal(got, []string{"t.Extra", "value"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestTwoWiresReturningOnePointerCloseItOnce(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	a, b := shared("t.A", v, false), shared("t.B", v, false)
	c := typewirego.NewContainer(bg())
	apply(t, c, a, b)
	mustResolve(t, c, a)
	mustResolve(t, c, b)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times", log.count("shared"))
	}
}

func TestPassThroughWireClosesItsImportsValueOnce(t *testing.T) {
	log := &orderLog{}
	inner := resWire("t.Inner", log)
	pass := typewirego.Of(typewirego.Opts[*res]{Token: "t.Pass",
		Imports: []typewirego.Import{{Name: "inner", Wire: inner}},
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (*res, error) {
			return typewirego.Dep(d, inner)
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, pass)
	mustResolve(t, c, pass)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"t.Inner"}) {
		t.Fatalf("closes = %v", got)
	}
}

func TestSharedValueClosedAfterEverythingCreatedAfterIt(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	owner := shared("t.Owner", v, false)
	middle := resWire("t.Middle", log)
	alias := shared("t.Alias", v, false)
	c := typewirego.NewContainer(bg())
	apply(t, c, owner, middle, alias)
	mustResolve(t, c, owner)
	mustResolve(t, c, middle)
	mustResolve(t, c, alias)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"t.Middle", "shared"}) {
		t.Fatalf("order = %v", got)
	}
}

// sliceCloser is a closer whose dynamic type is not comparable.
type sliceCloser []*orderLog

func (s sliceCloser) Close() error { s[0].add("slice"); return nil }

// boxed is a comparable type that can hold an incomparable value, so == on
// two boxes holding a slice panics.
type boxed struct {
	inner any
	log   *orderLog
}

func (b boxed) Close() error { b.log.add("boxed"); return nil }

func TestNonComparableValueIsNotDedupedAndDoesNotPanic(t *testing.T) {
	log := &orderLog{}
	s := sliceCloser{log}
	b := boxed{inner: []int{1}, log: log}
	c := typewirego.NewContainer(bg())
	ws := []typewirego.AnyWire{shared("t.S1", s, false), shared("t.S2", s, false),
		shared("t.B1", b, false), shared("t.B2", b, false)}
	apply(t, c, ws...)
	for _, w := range ws {
		mustResolve(t, c, w)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if log.count("slice") != 2 || log.count("boxed") != 2 {
		t.Fatalf("closes = %v, want each wire to close its own copy", log.get())
	}
}

func TestKeepOpenValueIsNeverClosedAndReportsNoCloser(t *testing.T) {
	log := &orderLog{}
	borrowed := shared("t.Stdout", newRes("stdout", log), true)
	c, o := observed(nil)
	apply(t, c, borrowed)
	mustResolve(t, c, borrowed)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if len(log.get()) != 0 {
		t.Fatal("a KeepOpen value was closed")
	}
	if ok := o.find(typewirego.EventResolveOK, "t.Stdout"); ok == nil || ok.Closer != "none" {
		t.Fatalf("resolve-ok = %+v, want closer none", ok)
	}
	if o.find(typewirego.EventCleanupStart, "t.Stdout") != nil {
		t.Fatal("a cleanup ran for a KeepOpen value")
	}
}

func TestKeepOpenAliasDemotesItsOwner(t *testing.T) {
	log := &orderLog{}
	v := newRes("client", log)
	owner := shared("t.Owner", v, false)
	borrow := shared("t.Borrow", v, true)
	c, o := observed(nil)
	apply(t, c, owner, borrow)
	mustResolve(t, c, owner)
	mustResolve(t, c, borrow)
	again := shared("t.Again", v, false)
	apply(t, c, again)
	mustResolve(t, c, again)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if len(log.get()) != 0 {
		t.Fatalf("a value a KeepOpen singleton returned was closed: %v", log.get())
	}
	if ok := o.find(typewirego.EventResolveOK, "t.Again"); ok == nil || ok.Closer != "none" {
		t.Fatalf("a later creation of a KeepOpen value reports %+v", ok)
	}
}

func TestKeepOpenNonComparableValueMustBePointerWrapped(t *testing.T) {
	log := &orderLog{}
	s := sliceCloser{log}
	c := typewirego.NewContainer(bg())
	a, b := shared("t.Borrowed", s, true), shared("t.Other", s, false)
	apply(t, c, a, b)
	mustResolve(t, c, a)
	mustResolve(t, c, b)
	_ = c.Teardown(bg())
	if log.count("slice") != 1 {
		t.Fatalf("an incomparable value cannot be recognized: want the other wire to close it once, got %v", log.get())
	}
	// Wrapped in a pointer, the value is recognized and kept open.
	log2 := &orderLog{}
	p := &sliceCloser{log2}
	c2 := typewirego.NewContainer(bg())
	a2, b2 := shared("t.Borrowed", p, true), shared("t.Other", p, false)
	apply(t, c2, a2, b2)
	mustResolve(t, c2, a2)
	mustResolve(t, c2, b2)
	_ = c2.Teardown(bg())
	if len(log2.get()) != 0 {
		t.Fatal("a pointer-wrapped KeepOpen value was closed")
	}
}

func TestTeardownDoesNotCloseTransients(t *testing.T) {
	log := &orderLog{}
	tr := typewirego.Of(typewirego.Opts[*res]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (*res, error) { return newRes("t.T", log), nil }})
	c := typewirego.NewContainer(bg())
	apply(t, c, tr)
	mustResolve(t, c, tr)
	mustResolve(t, c, tr)
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if len(log.get()) != 0 {
		t.Fatalf("transients were closed: %v", log.get())
	}
}

type closeErrVal struct{ log *orderLog }

func (v *closeErrVal) Close() error { v.log.add("close-err"); return nil }

type closeVal struct{ log *orderLog }

func (v *closeVal) Close() { v.log.add("close") }

type shutdownVal struct{ log *orderLog }

func (v *shutdownVal) Shutdown(context.Context) error { v.log.add("shutdown"); return nil }

type bothVal struct{ log *orderLog }

func (v *bothVal) Shutdown(context.Context) error { v.log.add("both-shutdown"); return nil }
func (v *bothVal) Close() error                   { v.log.add("both-close"); return nil }

type plainVal struct{}

func TestTeardownRunsTheClosersItRecognizesOnTheValue(t *testing.T) {
	log := &orderLog{}
	c, o := observed(nil)
	ws := []typewirego.AnyWire{
		shared("t.CloseErr", &closeErrVal{log}, false),
		shared("t.Close", &closeVal{log}, false),
		shared("t.Shutdown", &shutdownVal{log}, false),
		shared("t.Both", &bothVal{log}, false),
		shared("t.Plain", &plainVal{}, false),
	}
	apply(t, c, ws...)
	for _, w := range ws {
		mustResolve(t, c, w)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	want := []string{"both-shutdown", "shutdown", "close", "close-err"}
	if got := log.get(); !slices.Equal(got, want) {
		t.Fatalf("closers = %v, want %v", got, want)
	}
	closers := map[string]string{}
	for _, e := range o.got() {
		if e.Kind == typewirego.EventResolveOK {
			closers[e.Label] = e.Closer
		}
	}
	wantClosers := map[string]string{"t.CloseErr": "close-err", "t.Close": "close",
		"t.Shutdown": "shutdown-ctx", "t.Both": "shutdown-ctx", "t.Plain": "none"}
	for k, v := range wantClosers {
		if closers[k] != v {
			t.Errorf("%s closer = %q, want %q", k, closers[k], v)
		}
	}
}

func TestLifetimeEndsAfterItsOwnCleanup(t *testing.T) {
	var lifeB, lifeFailed context.Context
	var bEndedAtA error = errors.New("unset")
	var bLiveAtC error = errors.New("unset")
	a := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.A", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error { bEndedAtA = lifeB.Err(); return nil }}, nil
	}})
	// B has no closer: its Lifetime still ends at its own slot.
	b := typewirego.Of(typewirego.Opts[int]{Token: "t.B",
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (int, error) {
			lifeB = typewirego.Lifetime(d)
			return 1, nil
		}})
	// C is created after B, so it is cleaned before B's slot.
	cw := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.C", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error { bLiveAtC = lifeB.Err(); return nil }}, nil
	}})
	failing := typewirego.Of(typewirego.Opts[int]{Token: "t.F",
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (int, error) {
			lifeFailed = typewirego.Lifetime(d)
			return 0, errors.New("no")
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, a, b, cw, failing)
	mustResolve(t, c, a)
	mustResolve(t, c, b)
	mustResolve(t, c, cw)
	_, _ = c.Resolve(bg(), failing.Token())
	if lifeFailed.Err() == nil {
		t.Fatal("a failed creation's Lifetime did not end at once")
	}
	if lifeB.Err() != nil {
		t.Fatal("Lifetime ended before Teardown")
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if bLiveAtC != nil {
		t.Fatalf("B's Lifetime ended before B's slot (at C's cleanup it was %v)", bLiveAtC)
	}
	if !errors.Is(bEndedAtA, context.Canceled) {
		t.Fatalf("at A's cleanup B's Lifetime was %v, want ended after B's own slot", bEndedAtA)
	}
}

func TestTransientLifetimeEndsAfterTeardown(t *testing.T) {
	var lives []context.Context
	var atCleanup error = errors.New("unset")
	tr := typewirego.Of(typewirego.Opts[int]{Token: "t.T", Scope: typewirego.Transient,
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (int, error) {
			lives = append(lives, typewirego.Lifetime(d))
			return 1, nil
		}})
	s := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.S", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error { atCleanup = lives[0].Err(); return nil }}, nil
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, tr, s)
	mustResolve(t, c, tr)
	mustResolve(t, c, tr)
	mustResolve(t, c, s)
	if lives[0] != lives[1] {
		t.Fatal("two transients got different Lifetimes; they share the container's")
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if atCleanup != nil {
		t.Fatal("the transient Lifetime ended during the cleanup pass")
	}
	// Teardown finished every cleanup itself, so the container's Lifetime has
	// ended by the time it returns.
	if lives[0].Err() == nil {
		t.Fatal("the transient Lifetime outlived every cleanup")
	}
}

func TestShutdownHonouringItsContextIsWaitedFor(t *testing.T) {
	log := &orderLog{}
	s := typewirego.Of(typewirego.Opts[*shutdowner]{Token: "t.S", Creator: func(context.Context) (*shutdowner, error) {
		return &shutdowner{fn: func(ctx context.Context) error {
			<-ctx.Done()
			log.add("t.S")
			return nil
		}}, nil
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, s)
	mustResolve(t, c, s)
	ctx, cancel := context.WithTimeout(bg(), 20*time.Millisecond)
	defer cancel()
	if err := c.Teardown(ctx); err != nil {
		t.Fatalf("teardown = %v, want nil: the closer returned within the grace", err)
	}
	if log.count("t.S") != 1 {
		t.Fatal("Teardown did not wait for the Shutdown")
	}
}

func TestTeardownStopsWaitingForABlockedCloserAtItsDeadline(t *testing.T) {
	log := &orderLog{}
	dep := resWire("t.Dep", log)
	first := resWire("t.First", log)
	blocked := newRes("t.Blocked", log)
	blocked.block = make(chan struct{})
	bw := shared("t.Blocked", blocked, false, dep)
	c := typewirego.NewContainer(bg())
	apply(t, c, first, bw)
	mustResolve(t, c, first)
	mustResolve(t, c, bw)
	const bound = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(bg(), bound)
	defer cancel()
	start := time.Now()
	tt := timeoutErr(t, c.Teardown(ctx))
	// The pass waits one grace past its bound, then goes on.
	if el := time.Since(start); el > bound+typewirego.CleanupGrace+400*time.Millisecond {
		t.Fatalf("Teardown took %v, want about %v", el, bound+typewirego.CleanupGrace)
	}
	// t.First is due to start after the deadline, so it too is started and
	// left to finish on its own.
	if !slices.Equal(tt.Cleanups, []string{"t.Blocked", "t.First"}) || !slices.Equal(tt.HeldBack, []string{"t.Dep"}) {
		t.Fatalf("Cleanups %v HeldBack %v", tt.Cleanups, tt.HeldBack)
	}
	if log.count("t.Dep") != 0 || log.count("t.Blocked") != 0 {
		t.Fatalf("cleaned %v: the blocked value's import must be held back", log.get())
	}
	close(blocked.block)
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	got := log.get()
	if len(got) != 3 || got[2] != "t.Dep" || log.count("t.First") != 1 {
		t.Fatalf("final = %v, want each once and the held-back import last", got)
	}
}

type panicky struct{ block chan struct{} }

func (p *panicky) Close() error { <-p.block; panic("late close panic") }

func TestBlockedCloserPanickingLateIsReportedByWaitAndStats(t *testing.T) {
	p := &panicky{block: make(chan struct{})}
	w := shared("t.Panicky", p, false)
	c := typewirego.NewContainer(bg())
	apply(t, c, w)
	mustResolve(t, c, w)
	timeoutErr(t, c.Teardown(expiredCtx()))
	close(p.block)
	if err := c.Wait(ctxWithTimeout(t)); err == nil {
		t.Fatal("Wait lost the late panic")
	}
	if c.Stats().CleanupPanics != 1 {
		t.Fatalf("CleanupPanics = %d", c.Stats().CleanupPanics)
	}
}

func TestNeverBoundTokenAfterTeardownBeginsIsNotRegisteredAndClosing(t *testing.T) {
	// P1 and P9 both apply: a token that was never bound fails with
	// ErrWireNotRegistered, and once Teardown has begun the error also matches
	// ErrClosing, from every Resolver and GetCached alike.
	never := typewirego.Of(typewirego.Opts[string]{Token: "t.Never",
		Creator: func(context.Context) (string, error) { return "never", nil }}).Token()
	both := func(where string, err error) {
		t.Helper()
		if !errors.Is(err, typewirego.ErrWireNotRegistered) || !errors.Is(err, typewirego.ErrClosing) {
			t.Errorf("%s: got %v, want ErrWireNotRegistered and ErrClosing", where, err)
		}
		if got := typewirego.ErrClassOf(err); got != "not_registered" {
			t.Errorf("%s: class %q, want not_registered", where, got)
		}
	}

	// Before Teardown: not registered only.
	g := newGate()
	var creatorResolver typewirego.Resolver
	running := typewirego.Of(typewirego.Opts[string]{Token: "t.Running",
		CreateWith: func(_ context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			creatorResolver = r
			g.enter()
			return "running", nil
		}})
	var flushErr error
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error {
			_, flushErr = o.r.Resolve(bg(), never)
			return nil
		}
	})
	apply(t, c, running)
	if _, err := c.Resolve(bg(), never); !errors.Is(err, typewirego.ErrWireNotRegistered) || errors.Is(err, typewirego.ErrClosing) {
		t.Fatalf("open: got %v, want ErrWireNotRegistered only", err)
	}
	go c.Resolve(bg(), running.Token())
	waitClosed(t, g.started, "creator")

	// Closing: Teardown waits on the running creator.
	done := make(chan error, 1)
	go func() { done <- c.Teardown(bg()) }()
	eventually(t, "closing", func() bool {
		_, err := c.GetCached(running.Token())
		return errors.Is(err, typewirego.ErrClosing)
	})
	_, err := c.Resolve(bg(), never)
	both("Resolve while closing", err)
	_, err = c.GetCached(never)
	both("GetCached while closing", err)
	_, err = creatorResolver.Resolve(bg(), never)
	both("a creator's Resolve while closing", err)
	_, err = creatorResolver.GetCached(never)
	both("a creator's GetCached while closing", err)
	_, err = o.r.Resolve(bg(), never)
	both("the observer's Resolve while closing", err)
	g.open()
	if err := recv(t, done, "teardown"); err != nil {
		t.Fatal(err)
	}

	// Closed.
	_, err = c.Resolve(bg(), never)
	both("Resolve after Teardown", err)
	_, err = c.GetCached(never)
	both("GetCached after Teardown", err)
	both("the observer's Resolve in Flush", flushErr)
	if !errors.Is(flushErr, typewirego.ErrObserverResolve) {
		t.Errorf("the observer's Resolve in Flush: got %v, want a refusal too", flushErr)
	}
}

// panicCloser panics with v from Close, after block when it is set.
type panicCloser struct {
	v     any
	block chan struct{}
}

func (p *panicCloser) Close() error {
	if p.block != nil {
		<-p.block
	}
	panic(p.v)
}

func TestPanicErrorValueIsReachableThroughTheErrorChain(t *testing.T) {
	errBoom := errors.New("boom")
	panics := func(label string, scope typewirego.Scope, v any) typewirego.TypeWire[string] {
		return typewirego.Of(typewirego.Opts[string]{Token: label, Scope: scope,
			Creator: func(context.Context) (string, error) { panic(v) }})
	}

	// A creator panicking with an error: singleton and transient.
	c := typewirego.NewContainer(bg())
	single, trans := panics("t.Single", typewirego.Singleton, errBoom), panics("t.Trans", typewirego.Transient, errBoom)
	apply(t, c, single, trans)
	for _, w := range []typewirego.AnyWire{single, trans} {
		_, err := c.Resolve(bg(), w.Token())
		if !errors.Is(err, typewirego.ErrCreator) || !errors.Is(err, errBoom) {
			t.Errorf("%s: got %v, want ErrCreator wrapping the panic value", w.Token().Label(), err)
		}
	}

	// A runtime panic is reachable with errors.As.
	var nilMap map[string]int
	nilWrite := typewirego.Of(typewirego.Opts[string]{Token: "t.NilWrite",
		Creator: func(context.Context) (string, error) { nilMap["x"] = 1; return "", nil }})
	apply(t, c, nilWrite)
	_, err := c.Resolve(bg(), nilWrite.Token())
	var rerr runtime.Error
	if !errors.As(err, &rerr) {
		t.Errorf("got %v, want a runtime.Error through errors.As", err)
	}

	// A panic value that is not an error still fails with ErrCreator only.
	text := panics("t.Text", typewirego.Singleton, "text")
	apply(t, c, text)
	if _, err := c.Resolve(bg(), text.Token()); !errors.Is(err, typewirego.ErrCreator) {
		t.Errorf("got %v, want ErrCreator", err)
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}

	// A closer panicking with an error, reported by Teardown and by its event.
	c, o := observed(nil)
	closer := shared("t.Closer", &panicCloser{v: errBoom}, false)
	textCloser := shared("t.TextCloser", &panicCloser{v: "text"}, false)
	apply(t, c, closer, textCloser)
	mustResolve(t, c, closer)
	mustResolve(t, c, textCloser)
	if err := c.Teardown(bg()); !errors.Is(err, errBoom) {
		t.Errorf("Teardown = %v, want the panic value reachable", err)
	}
	// The event's class follows the chain to the panic value; a value that is
	// not an error leaves the panic itself innermost.
	for label, want := range map[string]string{"t.Closer": "*errors.errorString", "t.TextCloser": "*typewirego.panicError"} {
		if e := o.find(typewirego.EventCleanupFailed, label); e == nil || e.ErrClass != want {
			t.Errorf("%s cleanup-failed = %+v, want class %s", label, e, want)
		}
	}

	// A late closer panicking with an error, reported by Wait.
	late := &panicCloser{v: errBoom, block: make(chan struct{})}
	lw := shared("t.Late", late, false)
	c = typewirego.NewContainer(bg())
	apply(t, c, lw)
	mustResolve(t, c, lw)
	timeoutErr(t, c.Teardown(expiredCtx()))
	close(late.block)
	if err := c.Wait(ctxWithTimeout(t)); !errors.Is(err, errBoom) {
		t.Errorf("Wait = %v, want the panic value reachable", err)
	}
}

func TestBlockedCloserHoldsBackItsImportInTheObserverSet(t *testing.T) {
	log := &orderLog{}
	sink := resWire("t.Sink", log)
	blocked := newRes("t.Svc", log)
	blocked.block = make(chan struct{})
	svc := shared("t.Svc", blocked, false, sink)
	c, o := observed(func(o *recObserver) {
		o.flush = func(context.Context) error { log.add("flush"); return nil }
	})
	apply(t, c, svc)
	mustResolve(t, c, sink)
	if _, err := o.r.Resolve(bg(), sink.Token()); err != nil {
		t.Fatal(err)
	}
	mustResolve(t, c, svc)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.Sink"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	if got := log.get(); !slices.Equal(got, []string{"flush"}) {
		t.Fatalf("cleaned %v; the sink must wait for the blocked closer that imports it", got)
	}
	// The sink is held back, not cleaned after Flush: closed lists it once,
	// in HeldBack.
	sinkID, _ := typewirego.NodeIDOf(c, sink.Token())
	closed := o.find(typewirego.EventClosed, "")
	if closed == nil || !slices.Equal(closed.HeldBack, []string{sinkID}) || len(closed.Remaining) != 0 {
		t.Fatalf("closed = %+v, want %s in HeldBack only", closed, sinkID)
	}
	close(blocked.block)
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"flush", "t.Svc", "t.Sink"}) {
		t.Fatalf("final = %v", got)
	}
}

// releasingCloser is a main-pass value whose cleanup opens g and returns once
// the late creation has finished, so Teardown is the last participant.
func releasingCloser(g *gate, finished func() bool, saw *atomic.Bool) typewirego.TypeWire[*hookCloser] {
	return typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.Releaser", Creator: func(context.Context) (*hookCloser, error) {
		return &hookCloser{close: func() error {
			g.open()
			deadline := time.Now().Add(typewirego.CleanupGrace / 2)
			for !finished() && time.Now().Before(deadline) {
				time.Sleep(100 * time.Microsecond) // observes, never orders
			}
			saw.Store(finished())
			return nil
		}}, nil
	}})
}

func TestTeardownMainPassFinishingLastCleansHeldBackImports(t *testing.T) {
	log := &orderLog{}
	s := resWire("t.S", log)
	g := newGate()
	late := gated("t.Late", typewirego.Singleton, g, newRes("t.Late", log), false, s)
	var sawFinish atomic.Bool
	releaser := releasingCloser(g, func() bool { return log.count("t.Late") == 1 }, &sawFinish)
	c := typewirego.NewContainer(bg())
	apply(t, c, releaser, late)
	mustResolve(t, c, s)
	mustResolve(t, c, releaser)
	startLate(t, c, late, g)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !sawFinish.Load() {
		t.Fatal("the late creation did not finish during the main pass, so Teardown was not last")
	}
	if !slices.Equal(tt.HeldBack, []string{"t.S"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	got := log.get()
	if log.count("t.S") != 1 || log.count("t.Late") != 1 || got[len(got)-1] != "t.S" {
		t.Fatalf("closes = %v, want the held-back import cleaned once, last", got)
	}
}

func TestHeldBackCleanupAfterTeardownFinishesLastIgnoresTheTeardownContext(t *testing.T) {
	var sawErr error = errors.New("unset")
	cleaned := make(chan struct{})
	s := typewirego.Of(typewirego.Opts[*shutdowner]{Token: "t.S", Creator: func(context.Context) (*shutdowner, error) {
		return &shutdowner{fn: func(ctx context.Context) error {
			sawErr = ctx.Err()
			close(cleaned)
			return nil
		}}, nil
	}})
	var lateDone atomic.Bool
	g := newGate()
	late := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.Late",
		Imports: []typewirego.Import{{Name: "s", Wire: s}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (*hookCloser, error) {
			g.enter()
			return &hookCloser{close: func() error { lateDone.Store(true); return nil }}, nil
		}})
	var sawFinish atomic.Bool
	releaser := releasingCloser(g, lateDone.Load, &sawFinish)
	c := typewirego.NewContainer(bg())
	apply(t, c, releaser, late)
	mustResolve(t, c, s)
	mustResolve(t, c, releaser)
	startLate(t, c, late, g)
	timeoutErr(t, c.Teardown(expiredCtx()))
	if !sawFinish.Load() {
		t.Fatal("the late creation did not finish during the main pass, so Teardown was not last")
	}
	waitClosed(t, cleaned, "held-back cleanup")
	if sawErr != nil {
		t.Fatalf("held-back cleanup saw %v, want a context Teardown's cancellation never reaches", sawErr)
	}
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
}

func TestLateTransientHungCloseDoesNotBlockItsCaller(t *testing.T) {
	log := &orderLog{}
	v := newRes("t.T", log)
	v.block = make(chan struct{})
	g := newGate()
	tr := gated("t.T", typewirego.Transient, g, v, false)
	c := typewirego.NewContainer(bg())
	apply(t, c, tr)
	errc := startLate(t, c, tr, g)
	timeoutErr(t, c.Teardown(expiredCtx()))
	g.open()
	if err := recv(t, errc, "the late transient's caller"); !errors.Is(err, typewirego.ErrClosing) {
		t.Fatalf("caller = %v", err)
	}
	close(v.block)
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("t.T") != 1 {
		t.Fatal("late transient not closed once")
	}
}

func TestLateTransientIsClosedAfterItsCallerReturns(t *testing.T) {
	returned := make(chan struct{})
	var closedAfter atomic.Bool
	g := newGate()
	tr := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (*hookCloser, error) {
			g.enter()
			return &hookCloser{close: func() error {
				select {
				case <-returned:
					closedAfter.Store(true)
				case <-time.After(testTimeout):
				}
				return nil
			}}, nil
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, tr)
	go func() {
		_, _ = c.Resolve(bg(), tr.Token())
		close(returned)
	}()
	waitClosed(t, g.started, "transient")
	timeoutErr(t, c.Teardown(expiredCtx()))
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if !closedAfter.Load() {
		t.Fatal("the late transient was closed before its caller returned")
	}
}

func TestTransientFinishingDuringClosingIsClosedInThatPass(t *testing.T) {
	log := &orderLog{}
	g := newGate()
	tr := gated("t.T", typewirego.Transient, g, newRes("t.T", log), false)
	c := typewirego.NewContainer(bg())
	apply(t, c, tr)
	errc := startLate(t, c, tr, g)
	tdDone := make(chan error, 1)
	go func() { tdDone <- c.Teardown(bg()) }()
	eventually(t, "closing", func() bool { return !c.IsBound(typewirego.Token{}) && closingNow(c, tr) })
	g.open()
	if err := recv(t, errc, "caller"); !errors.Is(err, typewirego.ErrClosing) {
		t.Fatalf("caller = %v", err)
	}
	if err := recv(t, tdDone, "teardown"); err != nil {
		t.Fatal(err)
	}
	if log.count("t.T") != 1 {
		t.Fatal("the transient was not closed in Teardown's pass")
	}
}

// closingNow reports whether Teardown has begun, by probing a resolve.
func closingNow(c *typewirego.Container, w typewirego.AnyWire) bool {
	_, err := c.GetCached(w.Token())
	return errors.Is(err, typewirego.ErrClosing)
}

func TestTwoLateCreatorsShareOneHeldBackImport(t *testing.T) {
	log := &orderLog{}
	s := resWire("t.S", log)
	g1, g2 := newGate(), newGate()
	l1 := gated("t.L1", typewirego.Singleton, g1, newRes("t.L1", log), false, s)
	l2 := gated("t.L2", typewirego.Singleton, g2, newRes("t.L2", log), false, s)
	c := typewirego.NewContainer(bg())
	apply(t, c, l1, l2)
	mustResolve(t, c, s)
	startLate(t, c, l1, g1)
	startLate(t, c, l2, g2)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.S"}) || len(tt.Running) != 2 {
		t.Fatalf("%+v", tt)
	}
	g1.open()
	eventually(t, "l1", func() bool { return log.count("t.L1") == 1 })
	if log.count("t.S") != 0 {
		t.Fatal("shared import cleaned while a late creator still runs")
	}
	g2.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("t.S") != 1 || log.get()[2] != "t.S" {
		t.Fatalf("closes = %v", log.get())
	}
}

func TestValueATransientHandedOutBeforeAnyOwnerIsNotHeldBack(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	tr := typewirego.Of(typewirego.Opts[*res]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (*res, error) { return v, nil }})
	g := newGate()
	x := typewirego.Of(typewirego.Opts[int]{Token: "t.X",
		Imports: []typewirego.Import{{Name: "t", Wire: tr}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (int, error) {
			g.enter()
			return 1, nil
		}})
	owner := shared("t.Owner", v, false)
	c := typewirego.NewContainer(bg())
	apply(t, c, x, owner)
	startLate(t, c, x, g) // the transient hands v to x before anyone owns it
	mustResolve(t, c, owner)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if len(tt.HeldBack) != 0 {
		t.Fatalf("HeldBack = %v, want none", tt.HeldBack)
	}
	if log.count("shared") != 1 {
		t.Fatal("the owner was not closed in the main pass")
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
}

func TestRunningImporterHoldsAnOwnedValueItGotThroughATransient(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	owner := shared("t.Owner", v, false)
	tr := typewirego.Of(typewirego.Opts[*res]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(context.Context) (*res, error) { return v, nil }})
	g := newGate()
	x := gated("t.X", typewirego.Singleton, g, 1, false, tr)
	c := typewirego.NewContainer(bg())
	apply(t, c, owner, x)
	mustResolve(t, c, owner)
	startLate(t, c, x, g)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.Owner"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	if log.count("shared") != 0 {
		t.Fatal("closed under the running importer")
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatal("not closed once")
	}
}

func TestHeldBackClosureReachesThroughATransientFrame(t *testing.T) {
	log := &orderLog{}
	s := resWire("t.S", log)
	tr := typewirego.Of(typewirego.Opts[int]{Token: "t.T", Scope: typewirego.Transient,
		Imports:    []typewirego.Import{{Name: "s", Wire: s}},
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (int, error) { return 1, nil }})
	g := newGate()
	x := gated("t.X", typewirego.Singleton, g, 1, false, tr)
	c := typewirego.NewContainer(bg())
	apply(t, c, x)
	startLate(t, c, x, g)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.S"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("t.S") != 1 {
		t.Fatal("not cleaned once")
	}
}

func TestHeldBackCountsAValueARunningCreatorIsWaking(t *testing.T) {
	log := &orderLog{}
	wg := newGate()
	w := gated("t.W", typewirego.Singleton, wg, newRes("t.W", log), false)
	xg := newGate()
	x := typewirego.Of(typewirego.Opts[int]{Token: "t.X",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (int, error) {
			_, _ = r.Resolve(ctx, w.Token()) // waits on W's latch
			xg.enter()
			return 1, nil
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, x, w)
	// Hold X's waiter between W's latch opening and X taking the lock again.
	woke := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	typewirego.SetAfterWakeHook(c, func(tok typewirego.Token) {
		if tok == w.Token() {
			once.Do(func() { close(woke); <-resume })
		}
	})
	go c.Resolve(bg(), x.Token())
	waitClosed(t, wg.started, "w")
	eventually(t, "x on w's latch", func() bool { return typewirego.LatchWaiters(c, w.Token()) == 1 })
	wg.open()
	waitClosed(t, woke, "x's waiter to wake")
	if _, err := c.GetCached(w.Token()); err != nil {
		t.Fatalf("w not cached: %v", err)
	}
	// Teardown snapshots while X has not yet seen W's value.
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.W"}) {
		t.Fatalf("HeldBack = %v, want the value the running creator is waking on", tt.HeldBack)
	}
	if log.count("t.W") != 0 {
		t.Fatal("a value a running creator waits on was cleaned in the main pass")
	}
	close(resume)
	xg.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("t.W") != 1 {
		t.Fatal("not cleaned once")
	}
}

func TestHeldBackAliasHoldsBackItsOwner(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	owner := shared("t.Owner", v, false)
	alias := shared("t.Alias", v, false)
	g := newGate()
	x := gated("t.X", typewirego.Singleton, g, 1, false, alias)
	c := typewirego.NewContainer(bg())
	apply(t, c, owner, x)
	mustResolve(t, c, owner)
	mustResolve(t, c, alias)
	startLate(t, c, x, g)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.Owner", "t.Alias"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	if log.count("shared") != 0 {
		t.Fatal("the owner was closed while its alias is held back")
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatal("not closed once")
	}
}

func TestLateAliasLifetimeOutlivesSharedClose(t *testing.T) {
	var aliasLife context.Context
	var atClose error = errors.New("unset")
	v := &hookCloser{}
	v.close = func() error { atClose = aliasLife.Err(); return nil }
	owner := shared("t.Owner", v, false)
	g := newGate()
	alias := typewirego.Of(typewirego.Opts[*hookCloser]{Token: "t.Alias",
		Imports: []typewirego.Import{{Name: "owner", Wire: owner}},
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (*hookCloser, error) {
			aliasLife = typewirego.Lifetime(d)
			g.enter()
			return typewirego.Dep(d, owner)
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, alias)
	startLate(t, c, alias, g)
	timeoutErr(t, c.Teardown(expiredCtx()))
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if atClose != nil {
		t.Fatalf("the alias's Lifetime ended before the shared value closed: %v", atClose)
	}
	if aliasLife.Err() == nil {
		t.Fatal("the alias's Lifetime never ended")
	}
}

// closingThenLate drives a Teardown whose wait the test ends: creations
// finish during the wait until end() is called, after which the rest are late.
func closingTeardown(c *typewirego.Container) (end func(), result <-chan error) {
	ctx, cancel := context.WithCancel(bg())
	done := make(chan error, 1)
	go func() { done <- c.Teardown(ctx) }()
	return cancel, done
}

func TestLateCreationClosesNothingAClosingTransientOwns(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	tg, lg := newGate(), newGate()
	tr := gated("t.T", typewirego.Transient, tg, v, false)
	late := gated("t.Late", typewirego.Singleton, lg, v, false)
	c := typewirego.NewContainer(bg())
	apply(t, c, tr, late)
	terr := startLate(t, c, tr, tg)
	startLate(t, c, late, lg)
	end, result := closingTeardown(c)
	eventually(t, "closing", func() bool { return closingNow(c, tr) })
	tg.open()
	if err := recv(t, terr, "transient caller"); !errors.Is(err, typewirego.ErrClosing) {
		t.Fatalf("transient caller = %v, want ErrClosing", err)
	}
	end()
	timeoutErr(t, recv(t, result, "teardown"))
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times by Teardown, want once by the closing transient", log.count("shared"))
	}
	lg.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times, want once: the late creation closes nothing", log.count("shared"))
	}
}

func TestLateKeepOpenAliasDoesNotReclaimAnOwnedValue(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	owner := shared("t.Owner", v, false)
	g := newGate()
	borrow := gated("t.Borrow", typewirego.Singleton, g, v, true, owner)
	c := typewirego.NewContainer(bg())
	apply(t, c, borrow)
	mustResolve(t, c, owner)
	startLate(t, c, borrow, g)
	timeoutErr(t, c.Teardown(expiredCtx()))
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times: a late KeepOpen alias cannot reclaim an owned value", log.count("shared"))
	}
}

func TestLateKeepOpenTransientDoesNotOwnALateSingletonsValue(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	tg, sg := newGate(), newGate()
	tr := gated("t.T", typewirego.Transient, tg, v, true)
	s := gated("t.S", typewirego.Singleton, sg, v, false)
	c := typewirego.NewContainer(bg())
	apply(t, c, tr, s)
	terr := startLate(t, c, tr, tg)
	startLate(t, c, s, sg)
	timeoutErr(t, c.Teardown(expiredCtx()))
	tg.open()
	recv(t, terr, "transient caller")
	sg.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times, want once by the late singleton", log.count("shared"))
	}
}

func TestLatePassThroughLeavesSharedValueToHeldBackPass(t *testing.T) {
	log := &orderLog{}
	inner := resWire("t.Inner", log)
	g := newGate()
	pass := typewirego.Of(typewirego.Opts[*res]{Token: "t.Pass",
		Imports: []typewirego.Import{{Name: "inner", Wire: inner}},
		CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (*res, error) {
			g.enter()
			return typewirego.Dep(d, inner)
		}})
	yg := newGate()
	other := gated("t.Other", typewirego.Singleton, yg, newRes("t.Other", log), false, inner)
	c := typewirego.NewContainer(bg())
	apply(t, c, pass, other)
	perr := startLate(t, c, pass, g)
	startLate(t, c, other, yg)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.Inner"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	g.open()
	recv(t, perr, "pass-through caller")
	if log.count("t.Inner") != 0 {
		t.Fatal("the late pass-through closed the shared value while another importer still runs")
	}
	yg.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"t.Other", "t.Inner"}) {
		t.Fatalf("closes = %v, want the shared value once, in the held-back pass", got)
	}
}

func TestClosingTransientDoesNotDemoteOrDoubleCloseAnOwnedValue(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	owner := shared("t.Owner", v, false)
	kg, ug := newGate(), newGate()
	keep := gated("t.Keep", typewirego.Transient, kg, v, true)
	plain := gated("t.Plain", typewirego.Transient, ug, v, false)
	c, o := observed(nil)
	apply(t, c, owner, keep, plain)
	mustResolve(t, c, owner)
	kerr := startLate(t, c, keep, kg)
	perr := startLate(t, c, plain, ug)
	tdDone := make(chan error, 1)
	go func() { tdDone <- c.Teardown(bg()) }()
	eventually(t, "closing", func() bool { return closingNow(c, owner) })
	// The KeepOpen transient finishes first, then the plain one.
	kg.open()
	recv(t, kerr, "keep caller")
	ug.open()
	recv(t, perr, "plain caller")
	if err := recv(t, tdDone, "teardown"); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times, want once by its owner", log.count("shared"))
	}
	for _, e := range o.got() {
		if e.Kind == typewirego.EventCleanupStart && e.Label != "t.Owner" {
			t.Fatalf("cleanup ran for %s; only the owner closes the value", e.Label)
		}
	}
	if o.find(typewirego.EventCleanupStart, "t.Owner") == nil {
		t.Fatal("the owner did not close its value")
	}
}

func TestSingletonFinishingDuringClosingOwnsAClosingTransientsValue(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	tg, sg := newGate(), newGate()
	tr := gated("t.T", typewirego.Transient, tg, v, false)
	s := gated("t.S", typewirego.Singleton, sg, v, false)
	c, o := observed(nil)
	apply(t, c, tr, s)
	terr := startLate(t, c, tr, tg)
	serr := startLate(t, c, s, sg)
	tdDone := make(chan error, 1)
	go func() { tdDone <- c.Teardown(bg()) }()
	eventually(t, "closing", func() bool { return closingNow(c, s) })
	tg.open()
	recv(t, terr, "transient")
	sg.open()
	recv(t, serr, "singleton")
	if err := recv(t, tdDone, "teardown"); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times", log.count("shared"))
	}
	started := o.find(typewirego.EventCleanupStart, "")
	if started == nil || started.Label != "t.S" {
		t.Fatalf("the singleton should own the value and close it: %+v", started)
	}
}

func TestSingletonTakeoverKeepsAClosingTransientsHold(t *testing.T) {
	log := &orderLog{}
	v := newRes("shared", log)
	tg, sg, xg := newGate(), newGate(), newGate()
	tr := gated("t.T", typewirego.Transient, tg, v, false)
	s := gated("t.S", typewirego.Singleton, sg, v, false)
	gotT := make(chan struct{})
	x := typewirego.Of(typewirego.Opts[int]{Token: "t.X",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (int, error) {
			_, _ = r.Resolve(ctx, tr.Token()) // finishes during the wait: ErrClosing
			close(gotT)
			xg.enter()
			return 1, nil
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, tr, s, x)
	go c.Resolve(bg(), x.Token())
	waitClosed(t, tg.started, "transient inside x")
	serr := startLate(t, c, s, sg)
	end, result := closingTeardown(c)
	eventually(t, "closing", func() bool { return closingNow(c, s) })
	tg.open()
	waitClosed(t, gotT, "x's transient")
	sg.open()
	recv(t, serr, "singleton")
	end()
	tt := timeoutErr(t, recv(t, result, "teardown"))
	if !slices.Contains(tt.HeldBack, "t.S") {
		t.Fatalf("HeldBack = %v, want the singleton that took over the transient's value", tt.HeldBack)
	}
	if log.count("shared") != 0 {
		t.Fatal("closed under the running creation that imported the transient")
	}
	xg.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("shared") != 1 {
		t.Fatalf("closed %d times", log.count("shared"))
	}
}

func TestGetCachedThroughTheResolverIsHeldBackOnTimeout(t *testing.T) {
	log := &orderLog{}
	s := resWire("t.S", log)
	g := newGate()
	x := typewirego.Of(typewirego.Opts[int]{Token: "t.X",
		CreateWith: func(_ context.Context, _ typewirego.Deps, r typewirego.Resolver) (int, error) {
			if _, err := r.GetCached(s.Token()); err != nil {
				return 0, err
			}
			g.enter()
			return 1, nil
		}})
	c := typewirego.NewContainer(bg())
	apply(t, c, s, x)
	mustResolve(t, c, s)
	startLate(t, c, x, g)
	tt := timeoutErr(t, c.Teardown(expiredCtx()))
	if !slices.Equal(tt.HeldBack, []string{"t.S"}) {
		t.Fatalf("HeldBack = %v", tt.HeldBack)
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("t.S") != 1 {
		t.Fatal("not cleaned once")
	}
}
