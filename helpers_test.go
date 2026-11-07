package typewirego_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/typewirego/typewirego"
)

// testTimeout bounds every wait a test makes, so a bug fails the test instead
// of hanging it. It is never used to order goroutines.
const testTimeout = 10 * time.Second

func bg() context.Context { return context.Background() }

// recv waits for ch or fails the test.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// eventually polls cond; it is for observing state another goroutine settles,
// never for ordering goroutines.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// orderLog records cleanup order across goroutines.
type orderLog struct {
	mu    sync.Mutex
	items []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	l.items = append(l.items, s)
	l.mu.Unlock()
}

func (l *orderLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.items)
}

func (l *orderLog) count(s string) int {
	n := 0
	for _, it := range l.get() {
		if it == s {
			n++
		}
	}
	return n
}

// res is a value with Close() error that logs its name.
type res struct {
	name  string
	log   *orderLog
	err   error
	block chan struct{} // when set, Close waits for it
	in    chan struct{} // when set, closed as Close begins
	once  sync.Once
}

func newRes(name string, log *orderLog) *res { return &res{name: name, log: log} }

func (r *res) Close() error {
	if r.in != nil {
		r.once.Do(func() { close(r.in) })
	}
	if r.block != nil {
		<-r.block
	}
	r.log.add(r.name)
	return r.err
}

// resWire is a singleton wire returning a fresh *res.
func resWire(label string, log *orderLog, imports ...typewirego.AnyWire) typewirego.TypeWire[*res] {
	var imps []typewirego.Import
	for _, w := range imports {
		imps = append(imps, typewirego.Import{Name: w.Token().Label(), Wire: w})
	}
	return typewirego.Of(typewirego.Opts[*res]{Token: label, Imports: imps,
		CreateWith: func(context.Context, typewirego.Deps, typewirego.Resolver) (*res, error) {
			return newRes(label, log), nil
		}})
}

// gate is a creator that reports when it starts and waits for release.
type gate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate { return &gate{started: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) enter() {
	g.once.Do(func() { close(g.started) })
	<-g.release
}

func (g *gate) open() { close(g.release) }

func mustResolve(t *testing.T, c *typewirego.Container, w typewirego.AnyWire) any {
	t.Helper()
	v, err := c.Resolve(bg(), w.Token())
	if err != nil {
		t.Fatalf("resolve %s: %v", w.Token().Label(), err)
	}
	return v
}

func apply(t *testing.T, c *typewirego.Container, ws ...typewirego.AnyWire) {
	t.Helper()
	if err := typewirego.GroupOf(ws...).Apply(c); err != nil {
		t.Fatal(err)
	}
}

func expiredCtx() context.Context {
	ctx, cancel := context.WithCancel(bg())
	cancel()
	return ctx
}

func timeoutErr(t *testing.T, err error) *typewirego.TeardownTimeoutError {
	t.Helper()
	var tt *typewirego.TeardownTimeoutError
	if !errors.As(err, &tt) {
		t.Fatalf("error %v is not a *TeardownTimeoutError", err)
	}
	return tt
}

// recObserver records the events it receives. Hooks run inside OnEvent and
// Flush when set.
type recObserver struct {
	r typewirego.Resolver

	mu      sync.Mutex
	events  []typewirego.Event
	onEvent func(e typewirego.Event)
	flush   func(ctx context.Context) error
	flushed atomic.Int32
	inEvent atomic.Int32
	overlap atomic.Bool
}

func (o *recObserver) OnEvent(e typewirego.Event) {
	if o.inEvent.Add(1) != 1 {
		o.overlap.Store(true)
	}
	defer o.inEvent.Add(-1)
	o.mu.Lock()
	o.events = append(o.events, e)
	hook := o.onEvent
	o.mu.Unlock()
	if hook != nil {
		hook(e)
	}
}

func (o *recObserver) Flush(ctx context.Context) error {
	if o.inEvent.Load() != 0 {
		o.overlap.Store(true)
	}
	o.flushed.Add(1)
	if o.flush != nil {
		return o.flush(ctx)
	}
	return nil
}

func (o *recObserver) got() []typewirego.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

func (o *recObserver) kinds() []typewirego.EventKind {
	var out []typewirego.EventKind
	for _, e := range o.got() {
		out = append(out, e.Kind)
	}
	return out
}

// find returns the first delivered event of kind for label, or nil.
func (o *recObserver) find(kind typewirego.EventKind, label string) *typewirego.Event {
	for _, e := range o.got() {
		if e.Kind == kind && (label == "" || e.Label == label) {
			e := e
			return &e
		}
	}
	return nil
}

// observed builds a container with a recObserver; setup runs on it before
// NewContainer returns.
func observed(setup func(o *recObserver), opts ...typewirego.Option) (*typewirego.Container, *recObserver) {
	o := &recObserver{}
	if setup != nil {
		setup(o)
	}
	f := func(r typewirego.Resolver) typewirego.Observer {
		o.r = r
		return o
	}
	all := append([]typewirego.Option{typewirego.WithObserver(f)}, opts...)
	return typewirego.NewContainer(bg(), all...), o
}

func observedWith(setup func(o *recObserver), requires []typewirego.AnyWire, opts ...typewirego.Option) (*typewirego.Container, *recObserver) {
	o := &recObserver{}
	if setup != nil {
		setup(o)
	}
	f := func(r typewirego.Resolver) typewirego.Observer {
		o.r = r
		return o
	}
	all := append([]typewirego.Option{typewirego.WithObserver(f, requires...)}, opts...)
	return typewirego.NewContainer(bg(), all...), o
}

func pulledLabels(c *typewirego.Container) []string {
	var out []string
	for _, n := range c.Graph().Nodes {
		if n.ObserverPulled {
			out = append(out, n.Label)
		}
	}
	return out
}
