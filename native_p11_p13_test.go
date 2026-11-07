package typewirego_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/typewirego/typewirego"
)

func TestP11ConcurrentFirstResolveCreatesOnce(t *testing.T) {
	g := newGate()
	var calls atomic.Int32
	w := typewirego.Of(typewirego.Opts[*int]{Token: "t.Once", Creator: func(context.Context) (*int, error) {
		calls.Add(1)
		g.enter()
		return new(int), nil
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	const n = 32
	results := make(chan *int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			v, err := w.GetInstance(bg(), c)
			if err != nil {
				t.Error(err)
			}
			results <- v
		})
	}
	waitClosed(t, g.started, "creator start")
	// Every caller is on the one latch before the creation can finish.
	eventually(t, "all callers on the latch", func() bool { return typewirego.LatchWaiters(c, w.Token()) == n })
	g.open()
	wg.Wait()
	close(results)
	var first *int
	for v := range results {
		if first == nil {
			first = v
		}
		if v != first {
			t.Fatal("waiters received different values")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("creator ran %d times, want 1", calls.Load())
	}
}

func TestP11WaitersShareCreatorError(t *testing.T) {
	g := newGate()
	boom := errors.New("boom")
	var calls atomic.Int32
	w := typewirego.Of(typewirego.Opts[int]{Token: "t.Fail", Creator: func(context.Context) (int, error) {
		if calls.Add(1) > 1 {
			return 1, nil // a retry would succeed, so a waiter that retried would not see boom
		}
		g.enter()
		return 0, boom
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_, err := c.Resolve(bg(), w.Token())
			errs <- err
		})
	}
	waitClosed(t, g.started, "creator start")
	eventually(t, "all callers on the latch", func() bool { return typewirego.LatchWaiters(c, w.Token()) == n })
	g.open()
	wg.Wait()
	close(errs)
	for err := range errs {
		var ce *typewirego.CreatorError
		if !errors.As(err, &ce) || !errors.Is(err, boom) {
			t.Fatalf("waiter got %v, want the shared creator error", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("creator ran %d times, want 1 shared creation", calls.Load())
	}
}

func TestP11WaiterContextExpiryLeavesCreatorRunning(t *testing.T) {
	g := newGate()
	var creatorCtx context.Context
	var calls atomic.Int32
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.Slow", Creator: func(ctx context.Context) (string, error) {
		calls.Add(1)
		creatorCtx = ctx
		g.enter()
		return "done", nil
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	ctx, cancel := context.WithCancel(bg())
	errc := make(chan error, 1)
	go func() {
		_, err := c.Resolve(ctx, w.Token())
		errc <- err
	}()
	waitClosed(t, g.started, "creator start")
	cancel()
	if err := recv(t, errc, "waiter"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v, want context.Canceled", err)
	}
	if creatorCtx.Err() != nil {
		t.Fatal("the waiter's cancellation reached the creator")
	}
	g.open()
	if v := mustResolve(t, c, w); v != "done" {
		t.Fatalf("value = %v", v)
	}
	if calls.Load() != 1 {
		t.Fatalf("creator ran %d times, want 1", calls.Load())
	}
}

func TestP11CallerCancelDoesNotFailOtherWaiters(t *testing.T) {
	g := newGate()
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.Shared", Creator: func(ctx context.Context) (string, error) {
		g.enter()
		if err := ctx.Err(); err != nil {
			return "", err // a caller's cancel must not reach here
		}
		return "v", nil
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	ctx, cancel := context.WithCancel(bg())
	cancelled := make(chan error, 1)
	patient := make(chan any, 1)
	// The caller that will cancel starts the creation.
	go func() { _, err := c.Resolve(ctx, w.Token()); cancelled <- err }()
	waitClosed(t, g.started, "creator start")
	go func() { v, _ := c.Resolve(bg(), w.Token()); patient <- v }()
	eventually(t, "both callers on the latch", func() bool { return typewirego.LatchWaiters(c, w.Token()) == 2 })
	cancel()
	if err := recv(t, cancelled, "cancelled waiter"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v", err)
	}
	g.open()
	if v := recv(t, patient, "patient waiter"); v != "v" {
		t.Fatalf("patient waiter got %v, want the shared value", v)
	}
}

func TestP11TwoGoroutineCycleIsCircularDependencyError(t *testing.T) {
	xIn, yIn := make(chan struct{}), make(chan struct{})
	var y typewirego.TypeWire[string]
	x := typewirego.Of(typewirego.Opts[string]{Token: "t.X",
		CreateWith: func(ctx context.Context, _ typewirego.Deps, r typewirego.Resolver) (string, error) {
			close(xIn)
			<-yIn
			_, err := r.Resolve(ctx, y.Token())
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
	apply(t, c, x, y)
	ctx, cancel := context.WithTimeout(bg(), testTimeout)
	defer cancel()
	errs := make(chan error, 2)
	go func() { _, err := c.Resolve(ctx, x.Token()); errs <- err }()
	go func() { _, err := c.Resolve(ctx, y.Token()); errs <- err }()
	for range 2 {
		err := recv(t, errs, "root resolve")
		var cycle *typewirego.CircularDependencyError
		if !errors.As(err, &cycle) {
			t.Fatalf("got %v, want a circular dependency error", err)
		}
		p := cycle.Path
		labels := map[string]bool{"t.X": true, "t.Y": true}
		if len(p) != 3 || p[0] != p[2] || p[0] == p[1] || !labels[p[0]] || !labels[p[1]] {
			t.Fatalf("path = %v, want t.X -> t.Y -> t.X or t.Y -> t.X -> t.Y", p)
		}
	}
}

func TestP11CreatorPanicReleasesLatch(t *testing.T) {
	g := newGate()
	var calls atomic.Int32
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.Panics", Creator: func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			g.enter()
			panic("first call panics")
		}
		return "ok", nil
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	errs := make(chan error, 2)
	for range 2 {
		go func() { _, err := c.Resolve(ctxWithTimeout(t), w.Token()); errs <- err }()
	}
	waitClosed(t, g.started, "creator start")
	eventually(t, "a second caller on the latch", func() bool { return typewirego.LatchWaiters(c, w.Token()) == 2 })
	g.open()
	for range 2 {
		var ce *typewirego.CreatorError
		if err := recv(t, errs, "waiter"); !errors.As(err, &ce) || ce.Label != "t.Panics" {
			t.Fatalf("waiter got %v, want *CreatorError", err)
		}
	}
	v, err := c.Resolve(ctxWithTimeout(t), w.Token())
	if err != nil || v != "ok" {
		t.Fatalf("retry after the panic = %v, %v", v, err)
	}
}

func TestP12TeardownRejectsNewResolvesWithErrClosing(t *testing.T) {
	g := newGate()
	slow := typewirego.Of(typewirego.Opts[string]{Token: "t.Slow", Creator: func(context.Context) (string, error) {
		g.enter()
		return "slow", nil
	}})
	other := leafWire("t.Other")
	cached := leafWire("t.Cached")
	c := typewirego.NewContainer(bg())
	apply(t, c, slow, other, cached)
	mustResolve(t, c, cached)
	go c.Resolve(bg(), slow.Token())
	waitClosed(t, g.started, "slow creator")
	done := make(chan error, 1)
	go func() { done <- c.Teardown(bg()) }()
	// Teardown is waiting on the slow creation: Closing has begun.
	eventually(t, "closing", func() bool {
		_, err := c.GetCached(cached.Token())
		return errors.Is(err, typewirego.ErrClosing)
	})
	for _, w := range []typewirego.AnyWire{other, cached} {
		if _, err := c.Resolve(bg(), w.Token()); !errors.Is(err, typewirego.ErrClosing) {
			t.Fatalf("resolve %s during teardown = %v, want ErrClosing", w.Token().Label(), err)
		}
	}
	g.open()
	if err := recv(t, done, "teardown"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve(bg(), other.Token()); !errors.Is(err, typewirego.ErrClosing) {
		t.Fatalf("resolve after teardown = %v, want ErrClosing", err)
	}
}

func TestP12TeardownCancelsInFlightCreators(t *testing.T) {
	started := make(chan struct{})
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.Blocked", Creator: func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, w)
	errc := make(chan error, 1)
	go func() { _, err := c.Resolve(bg(), w.Token()); errc <- err }()
	waitClosed(t, started, "creator start")
	done := make(chan error, 1)
	go func() { done <- c.Teardown(bg()) }()
	if err := recv(t, done, "teardown (it hangs if the creator is never cancelled)"); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	err := recv(t, errc, "resolve")
	if !errors.Is(err, typewirego.ErrClosing) || !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve = %v, want ErrClosing carrying the cancellation", err)
	}
}

func TestP12CreationFinishingDuringWaitJoinsCleanupPass(t *testing.T) {
	log := &orderLog{}
	started := make(chan struct{})
	w := typewirego.Of(typewirego.Opts[*res]{Token: "t.Late", Creator: func(ctx context.Context) (*res, error) {
		close(started)
		<-ctx.Done() // Teardown has begun; finish anyway.
		return newRes("t.Late", log), nil
	}})
	cached := resWire("t.Cached", log)
	c := typewirego.NewContainer(bg())
	apply(t, c, cached, w)
	mustResolve(t, c, cached)
	errc := make(chan error, 1)
	go func() { _, err := c.Resolve(bg(), w.Token()); errc <- err }()
	waitClosed(t, started, "creator start")
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if err := recv(t, errc, "resolve"); !errors.Is(err, typewirego.ErrClosing) {
		t.Fatalf("caller got %v, want ErrClosing", err)
	}
	if got := log.get(); len(got) != 2 || got[0] != "t.Late" || got[1] != "t.Cached" {
		t.Fatalf("cleanup order = %v, want [t.Late t.Cached]", got)
	}
}

func TestP12TeardownWithExpiredContextAndRunningCreator(t *testing.T) {
	g := newGate()
	log := &orderLog{}
	w := typewirego.Of(typewirego.Opts[*res]{Token: "t.Stuck", Creator: func(context.Context) (*res, error) {
		g.enter()
		return newRes("t.Stuck", log), nil
	}})
	other := resWire("t.Other", log)
	c := typewirego.NewContainer(bg())
	apply(t, c, w, other)
	mustResolve(t, c, other)
	go c.Resolve(bg(), w.Token())
	waitClosed(t, g.started, "creator start")
	err := c.Teardown(expiredCtx())
	tt := timeoutErr(t, err)
	if len(tt.Running) != 1 || tt.Running[0] != "t.Stuck" {
		t.Fatalf("Running = %v", tt.Running)
	}
	if got := log.get(); len(got) != 1 || got[0] != "t.Other" {
		t.Fatalf("main pass cleaned %v, want [t.Other]", got)
	}
	if c.Stats().LateCreations != 1 {
		t.Fatalf("LateCreations = %d", c.Stats().LateCreations)
	}
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); err != nil {
		t.Fatal(err)
	}
	if log.count("t.Stuck") != 1 {
		t.Fatalf("late value cleaned %d times", log.count("t.Stuck"))
	}
}

func ctxWithTimeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(bg(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func TestP12LateCreationCleanedOnceAndReportedByWait(t *testing.T) {
	g := newGate()
	log := &orderLog{}
	closeErr := errors.New("late close failed")
	w := typewirego.Of(typewirego.Opts[*res]{Token: "t.Late", Creator: func(context.Context) (*res, error) {
		g.enter()
		r := newRes("t.Late", log)
		r.err = closeErr
		return r, nil
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, w)
	go c.Resolve(bg(), w.Token())
	waitClosed(t, g.started, "creator start")
	err := c.Teardown(expiredCtx())
	if errors.Is(err, closeErr) {
		t.Fatal("Teardown returned the late creation's cleanup error")
	}
	timeoutErr(t, err)
	g.open()
	if err := c.Wait(ctxWithTimeout(t)); !errors.Is(err, closeErr) {
		t.Fatalf("Wait = %v, want the late cleanup error", err)
	}
	if log.count("t.Late") != 1 {
		t.Fatalf("late value closed %d times", log.count("t.Late"))
	}
	if err := c.Wait(ctxWithTimeout(t)); !errors.Is(err, closeErr) {
		t.Fatal("a second Wait lost the error")
	}
}

func TestP12TeardownIsSerialized(t *testing.T) {
	log := &orderLog{}
	r := newRes("t.Blocking", log)
	r.block = make(chan struct{})
	r.in = make(chan struct{})
	w := typewirego.Of(typewirego.Opts[*res]{Token: "t.Blocking",
		Creator: func(context.Context) (*res, error) { return r, nil }})
	c := typewirego.NewContainer(bg())
	apply(t, c, w)
	mustResolve(t, c, w)
	first := make(chan error, 1)
	go func() { first <- c.Teardown(bg()) }()
	waitClosed(t, r.in, "first teardown's cleanup")
	var released atomic.Bool
	second := make(chan bool, 1)
	go func() {
		err := c.Teardown(bg())
		second <- err == nil && released.Load()
	}()
	eventually(t, "the second Teardown to queue", func() bool { return typewirego.TeardownsQueued(c) == 1 })
	released.Store(true)
	close(r.block)
	if err := recv(t, first, "first teardown"); err != nil {
		t.Fatal(err)
	}
	if !recv(t, second, "second teardown") {
		t.Fatal("the second Teardown returned before the first finished, or returned an error")
	}
	if log.count("t.Blocking") != 1 {
		t.Fatal("value closed more than once")
	}
}

type ctxKey struct{}

func TestP13SingletonCreatorRunsUnderContainerContext(t *testing.T) {
	var got context.Context
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.S", Creator: func(ctx context.Context) (string, error) {
		got = ctx
		return "s", nil
	}})
	c := typewirego.NewContainer(bg())
	apply(t, c, w)
	caller, cancel := context.WithCancel(context.WithValue(bg(), ctxKey{}, "caller"))
	if _, err := c.Resolve(caller, w.Token()); err != nil {
		t.Fatal(err)
	}
	cancel()
	if got.Value(ctxKey{}) != nil {
		t.Fatal("the singleton creator saw the caller's values")
	}
	if got.Err() != nil {
		t.Fatal("cancelling the caller cancelled the creator's context")
	}
	if err := c.Teardown(bg()); err != nil {
		t.Fatal(err)
	}
	if got.Err() == nil {
		t.Fatal("Teardown did not cancel the creator's context")
	}
}

func TestP13CallerContextBoundsOnlyItsWait(t *testing.T) {
	g := newGate()
	var calls atomic.Int32
	var creatorErr error = errors.New("unset")
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.Slow", Creator: func(ctx context.Context) (string, error) {
		calls.Add(1)
		g.enter()
		creatorErr = ctx.Err()
		return "slow", nil
	}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	ctx, cancel := context.WithTimeout(bg(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Resolve(ctx, w.Token()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded wait = %v, want DeadlineExceeded", err)
	}
	g.open()
	if v := mustResolve(t, c, w); v != "slow" {
		t.Fatalf("value = %v", v)
	}
	if calls.Load() != 1 || creatorErr != nil {
		t.Fatalf("creator ran %d times with ctx err %v: the caller's deadline reached the creation", calls.Load(), creatorErr)
	}
}

func TestP13TransientCreatorRunsUnderCallerContext(t *testing.T) {
	var got context.Context
	w := typewirego.Of(typewirego.Opts[string]{Token: "t.T", Scope: typewirego.Transient,
		Creator: func(ctx context.Context) (string, error) {
			got = ctx
			v, _ := ctx.Value(ctxKey{}).(string)
			return v, nil
		}})
	c := typewirego.NewContainer(bg())
	defer c.Teardown(bg())
	apply(t, c, w)
	caller, cancel := context.WithTimeout(context.WithValue(bg(), ctxKey{}, "caller"), time.Hour)
	v, err := c.Resolve(caller, w.Token())
	if err != nil || v != "caller" {
		t.Fatalf("transient saw %v, %v; want the caller's value", v, err)
	}
	if dl, ok := got.Deadline(); !ok || !dl.Equal(mustDeadline(caller)) {
		t.Fatal("the transient creator did not get the caller's deadline")
	}
	cancel()
	if got.Err() == nil {
		t.Fatal("cancelling the caller did not reach the transient creator's context")
	}
}

func mustDeadline(ctx context.Context) time.Time {
	dl, _ := ctx.Deadline()
	return dl
}
