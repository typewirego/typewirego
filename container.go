package typewirego

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

const (
	eventQueueCap      = 1024
	defaultFlushBudget = 5 * time.Second
	cleanupGrace       = 50 * time.Millisecond
)

type containerState int

const (
	stateOpen containerState = iota
	stateClosing
	stateClosed
)

// Container holds bindings, the singleton cache and the in-flight creations
// of one service run. It implements Binder and Resolver, and is passive except
// for running creators. Use NewContainer; a Container is single-use.
type Container struct {
	mu    sync.Mutex
	state containerState

	slots map[Token]*slot
	order []*slot

	// abortCtx is the context singleton creators run under; Teardown cancels
	// it when it begins.
	abortCtx    context.Context
	abortCancel context.CancelFunc
	// transLife is the Lifetime every transient shares; it ends once every
	// cleanup the container owns has run.
	transLife       context.Context
	transLifeCancel context.CancelFunc

	startSeq int
	// created lists, in creation (finish) order, the cached singletons and
	// the creations that finished during Teardown's wait.
	created   []*inst
	tokenInst map[Token]*inst
	// owners maps a value's identity to the creation that owns it.
	owners       map[any]*inst
	keepOpenVals map[any]bool
	// closingTrans maps a value to the transient that finished with it during
	// Teardown's wait and closes it.
	closingTrans map[any]*inst
	// transHolds links a transient token to the owners whose values it
	// returned, so importers of the token hold those owners back.
	transHolds map[Token]map[*inst]bool
	// transImports is the union of a transient token's runtime imports.
	transImports map[Token]map[Token]bool
	inFlight     map[*inst]bool

	tdStarted    bool
	tdQueued     int // Teardown calls waiting for the first
	tdDone       chan struct{}
	fixed        bool
	heldBack     map[*inst]bool
	participants int
	waitErrs     []error
	allDone      chan struct{}

	obsFactory  ObserverFactory
	obsRequires []AnyWire
	obs         *observerState
	appliedDone bool
	flushBudget time.Duration
	seq         uint64

	stats statsCounters

	// afterWake is a test hook run by a waiter after its wait ends and
	// before it takes the lock again.
	afterWake func(t Token)
}

type statsCounters struct {
	cleanupPanics         atomic.Uint64
	lateCreations         atomic.Uint64
	queueDrops            atomic.Uint64
	postClosedEvents      atomic.Uint64
	observerPanics        atomic.Uint64
	observerResolveErrors atomic.Uint64
	flushErrors           atomic.Uint64
}

// Stats is a snapshot of a container's monotonic counters. It is safe to take
// concurrently and after Teardown; late counts settle when Wait returns.
type Stats struct {
	// CleanupPanics counts closers that panicked; each also fails its cleanup.
	CleanupPanics uint64
	// LateCreations counts creations still running when Teardown timed out.
	LateCreations uint64
	// QueueDrops counts events dropped because the observer's queue was full,
	// or still queued when the drainer was abandoned at Teardown.
	QueueDrops uint64
	// PostClosedEvents counts events after closed, which are never delivered.
	PostClosedEvents uint64
	// ObserverPanics counts panics in the observer factory, OnEvent or Flush.
	ObserverPanics uint64
	// ObserverResolveErrors counts errors the observer's resolver returned.
	ObserverResolveErrors uint64
	// FlushErrors counts a Flush that failed, panicked or was still running
	// the grace after its allowance, or was skipped because OnEvent was still
	// running the grace after the drainer stop's allowance.
	FlushErrors uint64
}

// Stats returns a snapshot of the container's counters.
func (c *Container) Stats() Stats {
	s := &c.stats
	return Stats{
		CleanupPanics:         s.cleanupPanics.Load(),
		LateCreations:         s.lateCreations.Load(),
		QueueDrops:            s.queueDrops.Load(),
		PostClosedEvents:      s.postClosedEvents.Load(),
		ObserverPanics:        s.observerPanics.Load(),
		ObserverResolveErrors: s.observerResolveErrors.Load(),
		FlushErrors:           s.flushErrors.Load(),
	}
}

// slot is one bound token.
type slot struct {
	token       Token
	ordinal     int
	id          string
	wire        AnyWire
	core        *wireCore
	overridden  bool
	cached      *inst
	latch       *inst
	createdOnce bool
}

type closerKind int

const (
	closerNone closerKind = iota
	closerShutdownCtx
	closerCloseErr
	closerClose
)

func (k closerKind) String() string {
	switch k {
	case closerShutdownCtx:
		return "shutdown-ctx"
	case closerCloseErr:
		return "close-err"
	case closerClose:
		return "close"
	}
	return "none"
}

func closerOf(v any) closerKind {
	switch v.(type) {
	case nil:
		return closerNone
	case interface {
		Shutdown(context.Context) error
	}:
		return closerShutdownCtx
	case interface{ Close() error }:
		return closerCloseErr
	case interface{ Close() }:
		return closerClose
	}
	return closerNone
}

// identity returns v as a map key when its dynamic value is comparable.
func identity(v any) (key any, ok bool) {
	if v == nil || !reflect.TypeOf(v).Comparable() {
		return nil, false
	}
	defer func() {
		if recover() != nil {
			key, ok = nil, false
		}
	}()
	_ = v == v // panics for a comparable type holding an incomparable value
	return v, true
}

// inst is one creation: a singleton or transient run of a creator, and the
// record Teardown cleans it by.
type inst struct {
	slot     *slot
	core     *wireCore
	token    Token
	label    string
	scope    Scope
	keepOpen bool
	startSeq int
	idx      int
	started  time.Time
	done     chan struct{}
	fr       *frame

	value any
	err   error

	imports map[Token]bool
	waits   map[*inst]int
	// waiters counts the resolves currently waiting on this creation.
	waiters int

	closer    closerKind
	owns      bool
	owner     *inst
	aliasDeps []*inst
	cleaned   bool

	lifeCtx    context.Context
	lifeCancel context.CancelFunc

	closing        bool // finished during Teardown's wait
	late           bool // still running when Teardown's wait ended
	callerReturned chan struct{}
}

func (i *inst) cancelLife() {
	if i.lifeCancel != nil {
		i.lifeCancel()
	}
}

// result is what a caller or waiter of the creation receives.
func (i *inst) result() (any, error) {
	if i.closing || i.late {
		if i.err != nil {
			return nil, fmt.Errorf("%w: %w", ErrClosing, i.err)
		}
		return nil, closingErr(i.token)
	}
	if i.err != nil {
		return nil, i.err
	}
	return i.value, nil
}

// frame is one link of a resolve chain.
type frame struct {
	token  Token
	label  string
	parent *frame
	inst   *inst
	ended  bool
}

// Option configures NewContainer.
type Option func(*Container)

// WithFlushBudget sets the configured Flush allowance (P20). Negative is zero.
// Without it the budget is 5 seconds.
func WithFlushBudget(d time.Duration) Option {
	if d < 0 {
		d = 0
	}
	return func(c *Container) { c.flushBudget = d }
}

// WithObserver attaches the container's observer. factory runs inside
// NewContainer. requires are the wires the observer pulls; the first resolve
// that is not the observer's fails with ErrWireNotRegistered naming a required
// wire that is still unbound, until all of them are bound. A container takes
// one observer; a second WithObserver panics.
func WithObserver(f ObserverFactory, requires ...AnyWire) Option {
	if f == nil {
		panic("typewirego.WithObserver: nil factory")
	}
	for _, w := range requires {
		if w == nil {
			panic("typewirego.WithObserver: nil required wire")
		}
	}
	reqs := append([]AnyWire(nil), requires...)
	return func(c *Container) {
		if c.obsFactory != nil {
			panic("typewirego.WithObserver: a container takes one observer")
		}
		c.obsFactory = f
		c.obsRequires = reqs
	}
}

// NewContainer returns an empty, open container. ctx is for NewContainer's
// own work only: it is not retained, so none of its values reach a creator and
// cancelling it, as a signal.NotifyContext does when a graceful drain begins,
// fails no creation. Singleton creators run under an abort context that only
// Teardown cancels, when it begins (P12, P13, P18). With WithObserver, the
// observer's factory runs here and its drainer goroutine starts; the drainer
// exits in Teardown.
func NewContainer(ctx context.Context, opts ...Option) *Container {
	_ = ctx
	c := &Container{
		slots:        map[Token]*slot{},
		tokenInst:    map[Token]*inst{},
		owners:       map[any]*inst{},
		keepOpenVals: map[any]bool{},
		closingTrans: map[any]*inst{},
		transHolds:   map[Token]map[*inst]bool{},
		transImports: map[Token]map[Token]bool{},
		inFlight:     map[*inst]bool{},
		heldBack:     map[*inst]bool{},
		tdDone:       make(chan struct{}),
		allDone:      make(chan struct{}),
		flushBudget:  defaultFlushBudget,
	}
	c.abortCtx, c.abortCancel = context.WithCancel(context.Background())
	c.transLife, c.transLifeCancel = context.WithCancel(context.Background())
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.obsFactory != nil {
		c.startObserver()
	}
	return c
}

// Bind registers w for its token; the last binding wins. Rebinding never
// clears a cached value, so an override applied after its token is cached has
// no effect.
func (c *Container) Bind(w AnyWire) {
	if w == nil {
		panic("typewirego: Bind(nil)")
	}
	core := w.core()
	if core.token.isZero() {
		panic("typewirego: Bind of a wire with the zero Token")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.slots[core.token]
	if s == nil {
		s = &slot{token: core.token, ordinal: len(c.order) + 1}
		s.id = fmt.Sprintf("n%d", s.ordinal)
		c.slots[core.token] = s
		c.order = append(c.order, s)
	} else if s.wire != w {
		s.overridden = true
	}
	s.wire = w
	s.core = core
	c.emitLocked(Event{Kind: EventRegistered, NodeID: s.id, Label: s.token.Label(),
		Scope: core.scope, Overridden: s.overridden})
}

// IsBound reports whether t has a binding.
func (c *Container) IsBound(t Token) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slots[t] != nil
}

// GetCached returns t's cached singleton value without running a creator.
// It returns ErrWireNotRegistered for an unbound token, ErrClosing once
// Teardown has begun, and ErrNotResolved for a transient or a singleton not
// yet created.
func (c *Container) GetCached(t Token) (any, error) {
	return c.getCached(t, nil)
}

func (c *Container) getCached(t Token, from *frame) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.slots[t]
	if s == nil {
		return nil, notRegistered(t)
	}
	if from != nil {
		c.recordImportLocked(from, t)
	}
	if c.state != stateOpen {
		return nil, closingErr(t)
	}
	if s.core.scope == Transient || s.cached == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotResolved, t.Label())
	}
	return s.cached.value, nil
}

// Resolve returns t's value, creating it if needed. A singleton is created
// once per container; concurrent first resolves share that creation and its
// error, and each caller's ctx bounds only its own wait. With an observer,
// it first fails with ErrWireNotRegistered while a wire the observer requires
// is unbound.
func (c *Container) Resolve(ctx context.Context, t Token) (any, error) {
	return c.resolve(ctx, nil, t)
}
