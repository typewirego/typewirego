# typewirego specification

## Purpose and status

This is the contract for typewirego v0.x, the Go port of TypeWire. It restates,
in prose, the rules the package documentation in [`doc.go`](../doc.go) states
normatively. When this document and `doc.go` disagree, `doc.go` wins and this
document is the one to fix.

Each rule below names the tests that pin it. Rules P1–P10 are shared with the
Python ([typewirepy](https://github.com/typewirepy/typewirepy)) and TypeScript
([typewirets](https://github.com/typewirets/typewirets)) ports and are pinned
by the shared conformance cases in
[`testdata/conformance/cases.json`](../testdata/conformance/cases.json), run by
`TestConformanceCases`. Rules P11–P20 are Go-only today and are pinned by the
native tests listed in
[`testdata/conformance/native_tests.json`](../testdata/conformance/native_tests.json).

Error classes are contractual; error and panic message text is not.

## Model

A **wire** (`TypeWire[T]`, built by `Of`) is an immutable, package-level
description of how to create a `T`: a **token**, a **creator**, ordered
**imports** and a **scope**.

- A **token** identifies a wire inside a container. Tokens compare by identity:
  two wires built with the same label are two tokens. The label is diagnostic
  only. The zero `Token` is never bound.
- The **scope** is `Singleton` (the default: one value per container, cached)
  or `Transient` (a new value on every resolve, never cached).
- The **creator** is either `Opts.Creator`, `func(ctx) (T, error)`, for a leaf
  wire, or `Opts.CreateWith`, a `Creator[T]` that receives its resolved
  imports as `Deps` and the `Resolver` it may resolve more values through.
  `Dep(deps, wire)` reads one import; any wire that is not a declared import
  is an `ErrNotImported` error. `Of` panics on invalid options: both or neither
  creator, `Imports` without `CreateWith`, an unknown scope, a nil import wire,
  or two imports sharing a name. `Of` copies the imports.
- A **container** (`*Container`, from `NewContainer`) holds the bindings, the
  singleton cache and the in-flight creations of one service run. It is
  single-use.

Wires reach a container only through two **cutouts**:

- `Binder` (`Bind`, `IsBound`): what `Apply` registers through.
- `Resolver` (`Resolve`, `GetCached`): what `GetInstance` and creators resolve
  through. `GetCached` is a cache-only read and never runs a creator.

The container implements both and is otherwise passive: it never calls back
into wiring code except to run creators.

A **group** (`GroupOf`, `CombineGroups`, `Group.WithExtraWires`) is an
immutable ordered list of wires applied as a unit. Applying a group applies its
wires in order and stops at the first error. `GroupOf` panics on a nil wire.

**Apply** binds a wire's imports first, depth first in declaration order, then
the wire itself. An import whose token is already bound is skipped together
with its own imports, so an override applied first survives the original's
later Apply. A wire's `Apply` returns the first error an import's `Apply`
returns, and otherwise nil.

## P1 Late binding

Registration is late-binding and the last binding for a token wins. Applying a
wire registers its imports recursively, skipping any already bound.

- Go API: `Container.Bind`, `Container.IsBound`, `AnyWire.Apply`,
  `Group.Apply`, `TypeWire.WithCreator`.
- Imports resolve by token at resolve time, in declaration order, so a later
  override of an import is the one used.
- `WithCreator` returns a wire with the same token, scope, declared imports and
  `KeepOpen`. Its function receives `Deps` with no imports resolved (only
  `Lifetime` applies to it) and the `original` creator, which resolves the
  declared imports through the Resolver it is given, when and only if it is
  called. A test double therefore never dials the real dependency, while the
  declared imports stay registered.
- A token never bound fails to resolve with `ErrWireNotRegistered`. `Bind(nil)`
  panics.

Pinned by:

- case "applying a wire registers its imports recursively"
- case "a wire that was never applied is not registered"
- case "two wires with one label are two tokens"
- case "an override applied after the original, before any resolve, wins"
- case "an import is looked up at resolve time, so a later override of it is used"
- case "an override keeps the original's imports registered"
- case "an override that never calls the original does not resolve the original's imports"
- case "graph after apply lists declared nodes and import edges before any creator runs"
- case "graph renders two tokens with one label as two nodes"
- case "graph marks a wire bound again by an override as overridden"
- also `TestWithCreatorResolvesDeclaredImportsOnlyWhenOriginalRuns`,
  `TestWithCreatorKeepsKeepOpen`, `TestDepRejectsAWireOutsideTheImports`,
  `TestOfPanicsOnInvalidOptions`

## P2 Lazy creation

Creation is lazy: nothing runs before the first resolve, and a resolve runs
only the creators it needs. Apply, Bind and Graph run no creator.

- Go API: `Container.Resolve`, `TypeWire.GetInstance`.

Pinned by:

- case "apply runs no creator"
- case "the first resolve runs only the creators it needs"
- case "graph after apply lists declared nodes and import edges before any creator runs"
- also `TestGetCachedNeverRunsACreator`

## P3 Singleton cache

A singleton is created once per container, and later resolves return the
cached value, including zero values (`0`, `""`, `false`, `nil`). A nil
interface value resolves to the zero value of `T`.

- Go API: `Scope` `Singleton`, `Container.Resolve`, `Container.GetCached`.

Pinned by:

- case "a singleton is created once and later resolves return the cache"
- case "a singleton shared by two importers is created once"
- case "a singleton whose value is falsy is still cached"
- case "a singleton importing a transient captures one instance"
- case "a transient importing a singleton reuses the cached singleton"
- also `TestANilInterfaceValueResolvesToTheZeroValue`

## P4 Transients

A transient runs its creator on every resolve and nothing is cached. A
singleton that imports a transient captures the one instance its creation
received.

- Go API: `Scope` `Transient`. `GetCached` on a transient returns
  `ErrNotResolved`.

Pinned by:

- case "a transient runs its creator on every resolve"
- case "a singleton importing a transient captures one instance"
- case "a transient importing a singleton reuses the cached singleton"

## P5 Retry after failure

A failed creation caches nothing; the next resolve retries. A creator whose
import fails does not run, and fails with `*CreatorError` itself. A creator
panic, or a creator that calls `runtime.Goexit`, is recovered into
`*CreatorError` and releases its latch.

- Go API: `*CreatorError`, `ErrCreator`.

Pinned by:

- case "a failed creation is retried on the next resolve"
- case "a failed import fails the importer without running its creator"
- case "a failed creation caches nothing, so an override applied afterwards takes effect"
- also `TestP11CreatorPanicReleasesLatch`, `TestCreatorGoexitReleasesLatch`

## P6 Rebinding never clears a cache

Rebinding a token never clears its cache: an override applied after the
singleton is cached has no effect, and neither does an override of an import
applied after its importer is cached.

- Go API: `Container.Bind`.

Pinned by:

- case "an override applied after the original, before any resolve, wins"
- case "a failed creation caches nothing, so an override applied afterwards takes effect"
- case "an override applied after the singleton is cached has no effect"
- case "an override of an import applied after the importer is cached leaves the importer's value alone"

## P7 Cycles

A cycle is an error carrying the token path, never a hang. Wires are immutable
and `Of` copies the imports, so a declared import cycle cannot be built, and
there is no cycle check at Bind. A cycle can still arise at run time, when a
creator resolves through the Resolver it is handed.

- Go API: `*CircularDependencyError` (`Path`), `ErrCircularDependency`,
  `Creator`'s Resolver argument.
- The Resolver a creator receives carries the creator's resolve chain,
  independent of the context passed with it. A token already on the chain
  fails with `*CircularDependencyError`, whose `Path` holds the token labels
  from the root of the chain to the repeated token, for example
  `[t.B t.A t.C t.B]`.
- The chain ends when the creator returns. A creator that keeps its Resolver
  and resolves through it later starts a new chain, as a root resolve does: it
  gets cached values, its own included, and the closing refusals; the token is
  still recorded as the creator's runtime import. Every walk up a chain stops
  at a creator that has returned.
- The resolve that detects the cycle returns `*CircularDependencyError`; the
  creators above it fail with `*CreatorError` wrapping it, so `errors.Is` and
  `errors.As` reach it from the root.

Pinned by:

- case "a singleton shared by two importers is created once"
- case "a runtime resolve that closes a cycle fails with cycle and the label path"
- case "a wire that resolves itself fails with cycle"
- case "a cycle through an import is reported from the importer"
- case "a runtime resolve without a cycle succeeds"
- also `TestCreatorResolvingItsOwnTokenWhileRunningIsACycle`,
  `TestCreatorResolverOutlivesItsCreatorWithoutAFalseCycle`,
  `TestCreationBelowAnEndedCreatorResolvesItWithoutACycle`

## P8 Teardown order and cleanup by value

Teardown cleans created values in reverse creation order, once each. Cleanup is
a property of the value, checked in this order: `Shutdown(context.Context)
error`, `Close() error`, `Close()`. Any other value gets no cleanup. A value
with both `Shutdown` and `Close` is shut down, not closed.

- Go API: `Container.Teardown`, `Opts.KeepOpen`.
- Singleton values are cleaned. A transient that reached its caller is never
  retained, so it is never closed; a transient that finishes during Teardown's
  wait, or after it as a late creation, is cleaned (P12).
- **Ownership.** A wire owns what its creator creates. A value reachable
  through several wires (a pass-through wire returning an import's value, or
  two wires handing out one instance) is closed once: the first creation to
  finish with it owns it; the others are *aliases* and close nothing. The
  owner's slot is the earliest-created of theirs, the last that reverse
  creation order reaches, so the value outlives everything created after it.
  The owner is held back, or kept for the observer set, when any of the others
  would be. Identity is Go's `==` on the dynamic value; a value whose
  dynamic type is not comparable is not deduplicated, and each wire returning
  it closes it.
- A wire that attaches another import to a value it returns must wrap the value
  in a new type: the value keeps its first creator's slot, so the attached
  import is cleaned before it.
- **Borrowed values.** A wire that hands out a value it does not own sets
  `Opts.KeepOpen` (or wraps the value in a type with no closer). The container
  never closes a comparable value a `KeepOpen` singleton returns, whichever
  wire returning it finished first, and every creation of it reports closer
  `none` from then on. The exception is a `KeepOpen` creation that finishes
  after Teardown has timed out: it cannot reclaim a value another wire already
  owns, which that wire still closes. A `KeepOpen` transient protects only
  itself, not a singleton that owns the same value, and never owns a value,
  during Closing or after it. A `KeepOpen` value is otherwise a value with no
  closer: it can still be held back for a late creation that imports it, which
  delays only the end of its `Lifetime`. A borrowed
  value of a non-comparable type cannot be recognized: wrap it in a pointer, or
  make every wire that returns it `KeepOpen`. `WithCreator` keeps `KeepOpen`.

Pinned by:

- case "teardown cleans created singletons in reverse creation order"
- case "teardown cleans each created value once and skips what was never created"
- case "teardown follows creation order, not apply order"
- case "teardown of a container that created nothing cleans nothing"
- case "teardown reports every cleanup failure and still runs every cleanup"
- also `TestTeardownRunsTheClosersItRecognizesOnTheValue`,
  `TestTeardownDoesNotCloseTransients`,
  `TestTwoWiresReturningOnePointerCloseItOnce`,
  `TestPassThroughWireClosesItsImportsValueOnce`,
  `TestSharedValueClosedAfterEverythingCreatedAfterIt`,
  `TestAttachedImportIsCleanedBeforeTheValueItWasAttachedTo`,
  `TestNonComparableValueIsNotDedupedAndDoesNotPanic`,
  `TestKeepOpenValueIsNeverClosedAndReportsNoCloser`,
  `TestKeepOpenAliasDemotesItsOwner`,
  `TestKeepOpenNonComparableValueMustBePointerWrapped`

## P9 Single use

A container is single-use. Once Teardown begins, every resolve fails with
`ErrClosing`, and so does `GetCached`, except the observer Resolver's cache
hits and set members that P16 still serves.

- Go API: `ErrClosing`.

Pinned by:

- case "a resolve after teardown fails with closing"
- also `TestP12TeardownRejectsNewResolvesWithErrClosing`

## P10 Every cleanup runs, every failure is returned

Teardown runs every cleanup and returns the failures of every cleanup it
completed, joined with `errors.Join`. Failures of the cleanups that finish
after Teardown stopped waiting (late creations, calls left running, held-back
values) are returned by `Wait` instead (P19); Flush results are never
returned (P20). A closer that panics is recovered, fails its cleanup and is
counted in `Stats.CleanupPanics`.

- Go API: `Container.Teardown`'s error.

Pinned by:

- case "teardown reports every cleanup failure and still runs every cleanup"
- case "teardown with no cleanup failure returns no error"

## P11 Concurrent resolution

Concurrent first resolves of a singleton share one creation through a
per-token latch, and its waiters share its value or error. (That a caller's
context bounds only its own wait is P13.)

- Go API: `Container.Resolve` from many goroutines.
- Across goroutines, a resolve about to wait on another creation first walks
  the wait-for graph. If that creation transitively waits on the caller's own
  chain, the resolve fails with `*CircularDependencyError` instead of
  deadlocking. Transient creators run inline, so their waits count as their
  enclosing creator's. The error's `Path`, and the `cycle-detected` event's,
  still name every frame on the cycle, the waiting transient frames included.

Pinned by:

- `TestP11ConcurrentFirstResolveCreatesOnce`
- `TestP11WaitersShareCreatorError`
- `TestP11WaiterContextExpiryLeavesCreatorRunning`
- `TestP11CallerCancelDoesNotFailOtherWaiters`
- `TestP11TwoGoroutineCycleIsCircularDependencyError`
- `TestP11CreatorPanicReleasesLatch`
- also `TestThreeGoroutineWaitForCycleIsCircularDependencyError`,
  `TestWaitForCycleThroughATransientFrame`,
  `TestWaitBegunByAnEndedCreatorsGoroutineIsNoCycle`

## P12 Closing waits for in-flight creations

Teardown enters Closing, cancels the abort context in-flight singleton creators
run under, and waits for in-flight creations, bounded by its context.

- Go API: `Container.Teardown`, `Container.Wait`, `*TeardownTimeoutError`.
- A creation finishing during that wait, singleton or transient, is never
  cached or returned: its callers get `ErrClosing` and its value is cleaned in
  the same pass. Such a transient is kept only for its own cleanup and never
  claims or demotes an owner: it closes nothing when it is `KeepOpen` or
  another creation already owns its value, and otherwise closes the value
  once. If a singleton then finishes with the same value during the same wait,
  the singleton owns it and closes it at its own slot, takes over the
  transient's token (a *takeover*), so a running creation that imported that
  transient still holds the value back, and the transient closes nothing.
  After the wait the transient stays the owner, so a late creation returning
  the value closes nothing.
- If the wait times out, the remaining creations are *late*. Each late
  creation cleans its own value when it finishes; the values the late
  creations import are held back and cleaned once, after the last of them, and
  `Wait` reports those errors.
- Teardown calls are serialized: later calls wait for the first and return
  nil.

Pinned by:

- `TestP12TeardownRejectsNewResolvesWithErrClosing`
- `TestP12TeardownCancelsInFlightCreators`
- `TestP12CreationFinishingDuringWaitJoinsCleanupPass`
- `TestP12TeardownWithExpiredContextAndRunningCreator`
- `TestP12LateCreationCleanedOnceAndReportedByWait`
- `TestP12TeardownIsSerialized`
- also `TestTransientFinishingDuringClosingIsClosedInThatPass`,
  `TestSingletonFinishingDuringClosingOwnsAClosingTransientsValue`,
  `TestClosingTransientDoesNotDemoteOrDoubleCloseAnOwnedValue`

## P13 Container context for singleton creators

Singleton creators run on their own goroutine under the container's abort
context, not a caller's. It carries none of the caller's values and is
cancelled only when Teardown begins, never by the `NewContainer` ctx, so a
signal context ending at the start of a graceful drain fails no creation.
Transient creators run on the caller's goroutine under the caller's context.
A caller's context bounds only that caller's wait: the creation keeps running
and other waiters are unaffected.

- Go API: `NewContainer(ctx, ...)` (ctx is not retained), `Creator`'s ctx.

Pinned by:

- `TestP13SingletonCreatorRunsUnderContainerContext`
- `TestP13CallerContextBoundsOnlyItsWait`
- `TestP13TransientCreatorRunsUnderCallerContext`
- also `TestNewContainerContextLeavesCreatorsRunning`

## P14 The observer observes only

`WithObserver` attaches one `Observer`. It is not a wire: nothing imports it
and it imports nothing. Its factory gets a `Resolver` and nothing else, never
the container or a `Binder`, so it can observe but never change container
state. The factory runs inside `NewContainer`, where every resolve through that
Resolver fails with `ErrObserverResolve` and is counted.

- Go API: `WithObserver`, `ObserverFactory`, `Observer`, `Stats`.
- A panic in the factory, `OnEvent` or `Flush` is recovered, counted in
  `Stats.ObserverPanics`, and disables the observer for the container's life.
- Observer failures never change a resolve's or a teardown's result; they are
  visible only in `Stats`, except that an abandoned drainer or an overrunning
  Flush holds back the observer set, which Teardown then reports by returning
  `*TeardownTimeoutError` with the set in `HeldBack` (P17, P20).
- The drainer goroutine exits only in Teardown, so a container with an
  observer must be torn down.
- A second `WithObserver` panics.

Pinned by:

- `TestP14ObserverFactoryReceivesResolverOnly`
- `TestP14ObserverPanicIsRecoveredCountedAndDisables`
- `TestP14ObserverErrorNeverAltersResolveOrTeardown`
- `TestP14FactoryResolvingDuringNewContainerIsRecorded`
- also `TestObserverFactoryPanicDisablesTheObserver`, `TestWithObserverTwicePanics`

## P15 Ordered, non-blocking delivery

Events are appended to a bounded queue (1024 events) under the container
mutex, at the state change that causes them, with a sequence number. A full
queue drops the event and counts it in `Stats.QueueDrops`, so no container
operation ever waits on the observer. `Seq` numbers dropped events too, so gaps
count drops.

- Go API: `Event`, `EventKind`, `Observer.OnEvent`.
- One drainer goroutine per container delivers events, one at a time, outside
  every lock: `OnEvent` is never concurrent and never re-entered. An event
  caused by the observer's own resolve is delivered after the current
  `OnEvent` returns.
- `resolve-ok` is emitted at the moment the value is cached, so it precedes
  every event that used the cached value. `closing` precedes every cleanup
  event, and `closed` is the last event delivered.

Pinned by:

- `TestP15ObserverPullsSinkInsideFirstOnEvent`
- `TestP15EventsDeliveredInSequenceNeverReentrant`
- `TestP15FullQueueDropsAndCountsWithoutBlocking`
- `TestP15ResolveOkPrecedesEventsUsingCachedValue`
- `TestP15ClosingPrecedesCleanupAndClosedIsLast`
- `TestP15OnEventNeverOverlapsFlush`
- `TestP15EventOrderUnderConcurrentResolves`
- also `TestEventsCarryNodeIDsDurationsAndCloser`

## P16 The observer's Resolver is enforced

The observer's Resolver refuses transients, serves cache hits only until the
first resolve that is not the observer's (the window the tests call "during
Apply", though it is set by that first resolve, not by Apply) and again while
Teardown is closing, and refuses everything after closed. Refusals match
`ErrObserverResolve`; a cache miss also matches `ErrNotResolved`, and a refusal
after closed also matches `ErrClosing`.

- Go API: the `Resolver` an `ObserverFactory` receives.
- Between the first non-observer resolve and Closing, the observer's Resolver
  creates singletons like any root resolve, and may wait on another goroutine's
  latch.
- Once Teardown has fixed its passes, the observer set is frozen and the
  Resolver serves only its members, whether from the cache or at the end of a
  wait.
- The first non-observer resolve checks that the wires passed in
  `WithObserver`'s `requires` are bound, and fails with `ErrWireNotRegistered`
  naming one until they are.

Pinned by:

- `TestP16ObserverResolverRejectsTransients`
- `TestP16ObserverResolverServesCacheHitsOnlyDuringApply`
- `TestP16ObserverResolverServesCacheHitsOnlyWhileClosing`
- `TestP16ObserverResolverFailsAfterClosed`
- `TestP16ObserverPullWaitsOnAnotherGoroutinesLatch`
- `TestP16UnboundRequiredWireFailsFirstResolve`
- also `TestObserverSetIsFrozenAtTheTeardownSnapshot`

## P17 The observer set is cleaned last

The observer set is the tokens the observer's Resolver returned or is waiting
on, plus their runtime import closure, minus transients and minus the values a
late creation still holds. Teardown cleans it last, an exception to reverse
creation order: the main pass, the `closed` event, the drainer stop, `Flush`,
then the observer set.

- Go API: `Node.ObserverPulled`, `Event.Remaining`.
- A wait that ends without a value leaves nothing in the set.
- The held-back closure counts a value a running creator is waiting on, so a
  latch that opened just before the snapshot cannot leave that value in the
  main pass.
- Events after `closed` are counted in `Stats.PostClosedEvents`, never
  delivered and never an error.

Pinned by:

- `TestP17ObserverSetCleanedLastAfterFlush`
- `TestP17ObserverSetIncludesRuntimeImportClosure`
- `TestP17TeardownWithSinkCreatedAfterService`
- `TestP17HeldBackTokenExcludedFromObserverPass`
- `TestP17UndeliveredEventCountsInStatsNotError`
- also `TestCancelledObserverWaitLeavesNoObserverSetMember`,
  `TestObserverPullWakingAfterTheTeardownSnapshotIsInTheObserverSet`,
  `TestObserverPullRacingTeardownNeverLeavesTheSinkInTheMainPass`

## P18 Two contexts

Creators get two contexts. The `ctx` argument of a singleton creator is the
container's abort context, cancelled when Closing begins. A value that must
outlive Closing, such as a background flusher, takes `Lifetime(deps)`.

- Go API: `Lifetime`, `Deps`.
- A singleton's `Lifetime` ends after its own cleanup slot (also when it has
  no closer): right after it in the main pass, later if the value is held back
  or in the observer set, and at once if its creation fails. An alias's `Lifetime`
  ends no earlier than its owner's cleanup.
- Every transient shares one `Lifetime`, the container's, which ends once
  every cleanup the container owns has run.
- `Lifetime` of a zero `Deps` is `context.Background()`.

Pinned by:

- `TestP18ContainerContextCarriesNoCallerValues`
- `TestP18CreatorAbortContextCancelledAtClosing`
- `TestP18LifetimeContextEndsAfterOwnCleanup`
- also `TestLifetimeEndsAfterItsOwnCleanup`, `TestTransientLifetimeEndsAfterTeardown`,
  `TestLateAliasLifetimeOutlivesSharedClose`

## P19 Timeouts still run the whole path

A Teardown whose context ends while creations or cleanup calls are still
running still runs the whole path and returns `*TeardownTimeoutError`, joined
with the cleanup errors. Values still in use are held back and cleaned once by
the last finisher.

- Go API: `*TeardownTimeoutError` (`Running`, `HeldBack`, `Cleanups`, all
  token labels), `Container.Wait`, `Stats.LateCreations`.
- Cleanup is bounded: the main pass by the teardown context, the observer-set
  pass and Flush each by one Flush allowance (P20). Within a bounded pass each
  cleanup call runs on its own goroutine, and the pass waits on its calls until
  one shared grace of 50ms after its bound ends (or after the pass began, if
  the bound had already ended). A context that can never end, such as
  `context.Background()`, runs every call inline.
- A call still running at that deadline, or due to start after it, is left to
  finish on its own: Teardown lists it in `Cleanups` and holds back its
  runtime import closure from this and every later pass.
- Hold-back follows import tokens and value identity: a creation that received
  an owned value through a transient holds back the value's owner, and a
  held-back alias holds back its owner. Identity is recorded only once a
  creation owns the value: a value a transient handed to a running creation
  before any singleton owned it is not held back when a singleton later claims
  it, and is closed in the main pass. Have the transient import that
  singleton, or resolve the singleton before the transient hands the value
  out, so the hold follows the import. The link is per token, so a transient
  token that ever returned an owned value holds that owner for every running
  importer of the token, even one that received a different value: that
  over-holds, never closes early.
- The held-back values and each late creation's own value are cleaned under a
  context Teardown's cancellation never reaches, off the caller's goroutine.
  `Wait` blocks until every late call, late Flush and late creation has
  returned and the held-back values are cleaned, and returns the errors
  Teardown did not.

Pinned by:

- `TestP19TeardownTimeoutWithRunningCreatorWhoseImportIsCached`
- `TestP19TimeoutStillRunsWholeTeardownPath`
- `TestP19LateCreatorCleansOnlyItsOwnValue`
- `TestP19HeldBackClosureCleanedOnceByLastFinisher`
- `TestP19CreationFinishingDuringWaitIsClosingFailure`
- also `TestTeardownStopsWaitingForABlockedCloserAtItsDeadline`,
  `TestTwoLateCreatorsShareOneHeldBackImport`,
  `TestRunningImporterHoldsAnOwnedValueItGotThroughATransient`,
  `TestHeldBackCleanupAfterTeardownFinishesLastIgnoresTheTeardownContext`

## P20 The Flush budget

The Flush allowance is fixed when Teardown begins: the configured budget
(`WithFlushBudget`, default 5 seconds, negative is zero), or a quarter of the
time then left on the teardown context if less. It is applied as a fresh
timeout on a context that ignores the teardown context's cancellation, so it
survives a wait timeout. The drainer stop, `Flush` and the observer-set cleanup
each get one such allowance.

- Go API: `WithFlushBudget`, `Observer.Flush`, `Stats.FlushErrors`.
- A drainer still inside `OnEvent` the grace after its allowance is
  abandoned: its undelivered events are dropped and counted, and Flush is
  skipped and counted as a Flush error. A drainer between events is stopped:
  its backlog is dropped and counted, and Flush runs as usual.
- A Flush still running the grace after its allowance is counted and left to
  finish; `Wait` waits for it but never reports its result.
- An abandoned `OnEvent` or an overrunning Flush may still be using the
  observer set, so the set and its closure are held back and listed in
  `*TeardownTimeoutError`'s `HeldBack`; the `closed` event, queued before the
  drainer stop, lists them in `Remaining`, not `HeldBack`, when it is delivered
  at all. They are cleaned once, after that `OnEvent` returns and the drainer
  exits or that Flush returns, by the last participant under the late context;
  `Wait` waits for that.

Pinned by:

- `TestP20FlushBudgetIsQuarterOfRemainingTime`
- `TestP20FlushBudgetUsesConfiguredValueWithoutDeadline`
- `TestP20FlushBudgetSurvivesWaitTimeout`
- `TestP20ObserverSetCleanupGetsSecondAllowance`
- also `TestFlushBudgetArithmetic`, `TestFlushIgnoringItsContextReturnsAtTheAllowance`,
  `TestOverrunningFlushHoldsBackTheObserverSet`,
  `TestTeardownAbandonsADrainerStuckInOnEvent`,
  `TestDrainerBetweenEventsAtTheDeadlineIsStoppedAndFlushRuns`

## Graph export

`Container.Graph` returns the bound wires as nodes and their declared imports
as edges, before or after any creator runs. There is no cycle check: a declared
cycle cannot be built.

- Node ids are registration ordinals, `n1`, `n2`, …, assigned on a token's
  first Bind. Apply binds imports before the wire that declares them, so
  imports get lower ordinals. Ids are never labels, because two tokens may
  share a label.
- Nodes are ordered by ordinal; edges run from the import to the importer and
  are ordered by their endpoints' ordinals. An import whose token was never
  bound has no node and no edge.
- `Node.Created` reports a cached singleton or a transient created at least
  once; `Node.Overridden` a token bound again by a different wire (re-applying
  the same wire is not an override); `Node.ObserverPulled` a member of the
  observer set.
- `Graph.DOT` renders `digraph wires { ... }` with two-space indents: one line
  `  <id> [label=<Go-quoted label>]` per node, with `, observer_pulled=true`
  before the `]` for observer-set members, then one line `  <from> -> <to>`
  per edge, and a trailing newline. Labels shared by several nodes get a `#1`,
  `#2`, … suffix in node order.
- `Graph.JSON` is the `encoding/json` rendering of `Graph`; it never renders
  `null` for an empty list, so an empty graph is `{"nodes":[],"edges":[]}`.

Pinned by: the three `graph` conformance cases, and
`TestGraphGoldenScenario`, `TestGraphDOTAndJSONRenderDuplicateLabelsAsTwoNodes`,
`TestGraphDOTMarksObserverPulledNodes`, `TestGraphNodeOrderIsNumericPastNine`,
`TestGraphOfAnEmptyContainerRendersEmptyLists`,
`TestGraphSkipsAnImportThatWasNeverBound`,
`TestGraphReapplyingTheSameWireIsNotAnOverride`,
`TestGraphMarksATransientCreatedOnce`,
`TestGraphObserverSetExcludesTransientsAndHeldBackValues`.

## Errors

Error classes are the contract, never message text. `errors.Is` and `errors.As`
reach every sentinel and carrier through `*CreatorError` (which unwraps to its
`Cause`) and through `errors.Join`.

| Sentinel | Carrier | Meaning |
|---|---|---|
| `ErrCircularDependency` | `*CircularDependencyError` (`Path`) | A resolve that would wait on itself (P7, P11) |
| `ErrWireNotRegistered` | — | A token never bound, or a wire the observer requires is unbound (P1, P16) |
| `ErrNotResolved` | — | `GetCached` of a transient or an uncreated singleton; an observer cache miss |
| `ErrNotImported` | — | `Dep` of a wire that is not a declared import |
| `ErrClosing` | — | A resolve once Teardown has begun, other than what the observer's Resolver may still serve (P9, P12, P16) |
| `ErrTeardownTimeout` | `*TeardownTimeoutError` (`Running`, `HeldBack`, `Cleanups`) | Teardown stopped waiting for creations or calls (P19, P20) |
| `ErrCreator` | `*CreatorError` (`Label`, `Cause`) | A creator that failed or panicked, including one whose import failed (P5) |
| `ErrObserverResolve` | — | A resolve the observer's Resolver refuses (P14, P16) |

`Event.ErrClass` names an error's class: the first `errors.Is` match in the
order `context.Canceled` (`canceled`), `context.DeadlineExceeded`
(`deadline_exceeded`), `ErrCircularDependency` (`cycle`),
`ErrWireNotRegistered` (`not_registered`), `ErrClosing` (`closing`),
`ErrTeardownTimeout` (`teardown_timeout`), `ErrNotResolved` (`not_resolved`),
`ErrNotImported` (`not_imported`), `ErrObserverResolve` (`observer_resolve`),
`ErrCreator` (`creator`); otherwise the `%T` of the innermost error, following
`Unwrap() error` and the first element of `Unwrap() []error`. A nil error has
class `""`. Pinned by `TestErrClassMatchesSentinelsBeforeTheInnermostType` and
`TestErrorCarriersMatchTheirSentinels`.

## Differences from the other ports

- **Go-only rules.** P11–P20 (concurrent resolution, the abort and lifetime
  contexts, bounded teardown, and the observer) exist only in Go today. They
  are pinned by Go-native tests, not by the shared suite.
- **Cases that run on fewer than three ports.** Of the 35 shared cases, Go
  runs 34:
    - "a one-argument override drops the original's imports" runs on Python
      only (rationale `introspection`): Python's one-argument `with_creator`
      inspects the function. Go's `WithCreator` always keeps the declared
      imports.
    - The teardown cases (P8, P9, P10) run on Go and Python (rationale
      `capability-absent`): TypeScript has no teardown yet.
    - The three `graph` cases run on Go only (rationale `capability-absent`):
      neither sibling has graph export yet.
- **Cleanup.** Go has no teardown registration API: cleanup is a property of
  the value (`Shutdown(ctx) error`, `Close() error`, `Close()`), as is
  idiomatic in Go.
- **Creators.** A Go creator receives the `Resolver`, so `CreateWith` needs no
  `Imports`, unlike typewirepy.
