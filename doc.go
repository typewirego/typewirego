// Package typewirego is dependency injection for Go services, with the same
// semantics as the Python (github.com/typewirepy/typewirepy) and TypeScript
// (github.com/typewirets/typewirets) libraries, so one wiring design reads the
// same in all three. A shared conformance suite in testdata/conformance holds the
// ports to that contract; docs/SPEC.md states it in prose.
//
// # Wires and containers
//
// A wire (TypeWire[T], built by Of) is an immutable, package-level description
// of how to create a T: a token, a creator, ordered imports and a scope. Wires
// talk to a container only through two cutouts: Apply binds through a Binder, and
// GetInstance resolves through a Resolver. The Container implements both and is
// otherwise passive; it never calls back into wiring code except to run creators.
//
//   - Binding is late and the last binding for a token wins (P1). Applying a wire
//     binds each import not already bound, so an override applied first survives
//     the original's later Apply.
//   - Nothing is created until the first resolve (P2). A singleton is created
//     once per container, then cached, including zero values (P3); a transient
//     runs its creator on every resolve (P4). A failed creation caches nothing,
//     so the next resolve retries (P5). Rebinding a token never clears its cache,
//     so an override after the value is cached has no effect (P6).
//   - Imports resolve by token at resolve time, in declaration order, so a later
//     override of an import is the one used. WithCreator keeps the declared
//     imports but resolves them only if the override calls the original, so a test
//     double never dials the real dependency.
//   - Tokens compare by identity; the label is diagnostic only.
//
// # Cycles
//
// Wires are immutable and Of copies the imports, so a declared import cycle
// cannot be built. A cycle can still arise at run time, when a creator resolves
// through the Resolver it is handed (P7). That Resolver carries the creator's
// resolve chain, independent of the context passed with it, and a token
// already on the chain fails with *CircularDependencyError instead of hanging.
// The chain ends when the creator returns: a creator that keeps the Resolver
// and resolves through it later starts a new chain, as a root resolve does,
// so it gets cached values, its own included, and the closing refusals; the token
// is still recorded as the creator's runtime import. Every walk up a chain stops
// at a creator that has returned, since it waits on nothing. Across goroutines,
// a resolve about to wait on another creation first walks the wait-for graph;
// if that creation transitively waits on the caller's own chain, the resolve fails
// with the same error (P11).
//
// # Concurrency and contexts
//
// Concurrent first resolves of a singleton share one creation through a per-token
// latch, and its waiters share its value or error (P11). Singleton creators run
// on their own goroutine under the container's abort context, which carries none
// of the caller's values and is cancelled only when Teardown begins, never by the
// NewContainer ctx, so a signal context ending at the start of a graceful drain
// fails no creation; a caller's context bounds only that caller's wait (P13).
// Transient creators run on the caller's goroutine under the caller's context.
// A creator panic is recovered into *CreatorError and releases the latch.
// A value that needs a context outliving Closing, such as a background flusher,
// takes Lifetime(deps), which ends only after that value's own cleanup.
// A transient value's Lifetime is the container's, which ends once every cleanup
// the container owns has run.
//
// # Teardown
//
// Cleanup is a property of the value, as is idiomatic in Go: a value with
// Shutdown(ctx) error, Close() error or Close() is cleaned with it, and any other
// value gets no cleanup (P8). Singleton values are cleaned; the container never
// retains a transient that reached its caller, so it never closes one. Teardown
// cleans in reverse creation order, once, and returns every cleanup failure joined
// (P10); the container is single-use afterwards (P9).
//
// A wire owns what its creator creates, and the container assumes it does:
// it closes every singleton value with a closer unless the wire says otherwise.
// A value reachable through several wires, such as a pass-through wire returning
// an import's value or two wires handing out one instance, is closed once:
// the first creation to finish with it owns it, and the others close nothing.
// The owner's slot is the last of theirs that reverse creation order reaches,
// so the value outlives everything created after it, and the owner is held back,
// or kept for the observer set, when any of the others would be. That includes
// a transient: a creation that received an owned value through a transient holds
// back the value's owner, as if it had imported the owner, because hold-back
// follows value identity as well as import tokens. Identity is recorded only once
// a creation owns the value: a value a transient handed to a running creation
// before any singleton owned it is not held back when a singleton later claims it,
// and is closed in the main pass. Have the transient import that singleton,
// or resolve the singleton before the transient hands the value out, so the
// hold follows the import. The link is per token, so a transient token that ever
// returned an owned value holds that owner for every running importer of the
// token, even one that received a different value; that over-holds, never closes
// early. A wire that attaches another import to the value it returns must wrap the
// value in a new type rather than mutate and return it: the value keeps its first
// creator's slot, so that import is cleaned before it. Identity is Go's == on the
// dynamic value: pointers compare by address, other comparable values by value.
// A value whose dynamic type is not comparable is not deduped, and each wire that
// returns it closes it.
//
// A wire that hands out a value it does not own, such as os.Stdout or a shared
// client passed in from the entrypoint, sets Opts.KeepOpen or wraps the value
// in a type with no closer. The container never closes a comparable value that
// a KeepOpen singleton returns, whichever wire returning it finished first,
// and every creation of it reports closer "none" from then on; a KeepOpen
// transient protects only itself, not a singleton that owns the same value.
// The exception is a KeepOpen creation that finishes after Teardown has timed out:
// it cannot reclaim a value another wire already owns, which that wire still
// closes. A KeepOpen transient never owns a value, during Closing or after it,
// so a singleton that returns the same value later still owns and closes it once.
// A borrowed value of a non-comparable type cannot be recognized, so it must
// be wrapped in a pointer, or every wire that returns it must be KeepOpen.
// A KeepOpen value is otherwise a value with no closer: it can still be held back
// for a late creation that imports it, which delays only the end of its Lifetime.
// WithCreator keeps KeepOpen.
//
// Teardown enters Closing, cancels the abort context and waits for in-flight
// creations, bounded by its context. A creation finishing during that wait,
// singleton or transient, is never cached or returned: it is cleaned in the
// same pass, and its callers get ErrClosing. Such a transient is kept only for
// its own cleanup and never claims or demotes an owner: it closes nothing when
// it is KeepOpen or another creation already owns its value, and otherwise
// closes the value once. If a singleton then finishes with that same value
// during the same wait, the singleton owns it and closes it at its own slot,
// takes over the transient's token, so a running creation that imported that
// transient still holds the value back, and the transient closes nothing; after
// the wait the transient stays the owner, so a late creation returning the value
// closes nothing. If the wait times out, Teardown still cleans everything except
// the values the running creations import, and returns *TeardownTimeoutError;
// each late creation cleans its own value when it finishes, the held-back imports
// are cleaned once after the last of them, and Wait reports those errors (P12).
//
// Cleanup is bounded too. The main pass is bounded by the teardown context,
// the observer-set pass and Flush each by one Flush allowance (see below).
// Within a bounded pass each cleanup call runs on its own goroutine, and the pass
// waits on its calls until one shared grace (50ms) after its bound ends, or after
// the pass began if the bound had already ended, so a closer that returns promptly
// or honours its context is waited for and the pass is over by then. A call still
// running at that deadline, or due to start after it, is left to finish on its
// own: Teardown lists it in *TeardownTimeoutError's Cleanups, holds back its
// runtime import closure from this and every later pass, so it never outlives a
// value it imports that was created before it, and goes on with the next value. A
// context that can never end, such as context.Background, runs every call inline.
// The held-back values and each late creation's own value are cleaned under a
// context Teardown's cancellation never reaches, off the caller's goroutine.
// Wait blocks until every such call, late Flush and late creation has returned
// and the held-back values are cleaned, and returns the errors Teardown did not;
// closer panics are counted in Stats either way.
//
// # Graph export
//
// Graph returns the bound wires as nodes and their declared imports as edges,
// before or after any creator runs, with DOT and JSON renderings. Node ids are
// registration ordinals (n1, n2, …), never labels, because two tokens may share
// a label; nodes and edges are ordered by those ordinals, so the output is stable
// for a given Apply order. An import whose token was never bound has no node and
// no edge. There is no cycle check at Bind: wires are immutable and Of copies
// their imports, so a declared cycle cannot be built.
//
// # The observer
//
// WithObserver attaches one Observer that receives every lifecycle event
// (P14-P20). It is not a wire: nothing imports it, it imports nothing,
// and it holds a single cutout, a Resolver, never the container or a Binder,
// so it can observe but never change container state. Its factory runs inside
// NewContainer, where every resolve through that Resolver fails and is counted.
// The Resolver records each token it returns, and each token it waits on for as
// long as the wait lasts, so a wait that ends without a value leaves nothing
// behind. It enforces P16: it refuses transients, serves cache hits only until the
// first resolve that is not the observer's and again while Teardown is closing,
// and refuses everything after closed. Once Teardown has fixed its passes,
// the observer set is frozen and the Resolver serves only its members, whether
// from the cache or at the end of a wait, because any other value may already be
// cleaned. The first non-observer resolve also checks that the wires the observer
// requires are bound, and fails with ErrWireNotRegistered naming one until they
// are, instead of silently recording nothing.
//
// Events are appended to a bounded queue under the container mutex, at the
// state change that causes them, with a sequence number; a full queue drops the
// event and counts it, so no container operation ever waits on the observer.
// One drainer goroutine per container delivers them, one at a time and outside
// every lock, so OnEvent is never concurrent and never re-entered: an event caused
// by the observer's own resolve is delivered after the current OnEvent returns.
// A synchronous emit was rejected because the observer's first pull would re-enter
// OnEvent and wait on its own latch. A panic in the factory, OnEvent or Flush
// is recovered, counted, and disables the observer for the container's life;
// observer failures never change a resolve's or a teardown's result and are
// visible only in Stats, except that an abandoned drainer or a Flush overrunning
// its allowance holds back the observer set, which Teardown then reports in
// *TeardownTimeoutError's HeldBack.
//
// Teardown cleans the observer set last, an exception to reverse creation order:
// the tokens the observer's resolver returned or waits on plus their runtime
// import closure, minus transients and the values a late creation still holds
// (P17). The held-back closure likewise counts a value a running creator is
// waiting on, so a latch that opened just before the snapshot cannot leave
// that value in the main pass. The order is the main pass, the closed event,
// the drainer stop, Flush, then the observer set. Events after closed are counted,
// not delivered. The Flush allowance is fixed when Teardown begins, as the
// configured budget or a quarter of the time then left on its context if less, and
// is applied as a fresh timeout on a context that ignores the teardown context's
// cancellation, so it survives a wait timeout (P20). The drainer stop, Flush and
// the observer-set cleanup each get one such allowance. A drainer still inside
// OnEvent the grace after its allowance is abandoned: its undelivered events
// are dropped and counted, and Flush is skipped and counted as a Flush error,
// since OnEvent never overlaps Flush. A drainer between events then never calls
// OnEvent again: its backlog is dropped and counted, and Flush runs as usual.
// A Flush still running the grace after its allowance is counted in Stats and left
// to finish; Wait waits for it but never reports its result. An abandoned OnEvent
// or an overrunning Flush may still be using the observer set, so the set and
// its runtime import closure are held back, as for an overrunning cleanup call:
// Teardown lists them in *TeardownTimeoutError's HeldBack, and they are cleaned
// once, after that OnEvent returns and the drainer exits or that Flush returns,
// by the last participant under the late context; Wait waits for that. The closed
// event was queued before the drainer stop, so it lists them in Remaining,
// not HeldBack, when it is delivered at all. The drainer goroutine exits only in
// Teardown, so a container with an observer must be torn down.
//
// # Errors
//
// Error classes are the contract, never message text. Each sentinel
// (ErrCircularDependency, ErrWireNotRegistered, ErrClosing, ErrTeardownTimeout,
// ErrCreator) has a typed carrier where it carries data, and errors.Is and
// errors.As reach them through *CreatorError and errors.Join.
package typewirego
