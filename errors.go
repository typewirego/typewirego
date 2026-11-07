package typewirego

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels for errors.Is. The carriers below match their sentinel through
// an Is method, so errors.Is and errors.As both work through wrapping and
// errors.Join.
var (
	ErrCircularDependency = errors.New("typewirego: circular dependency")
	ErrWireNotRegistered  = errors.New("typewirego: wire not registered")
	ErrNotResolved        = errors.New("typewirego: wire not resolved")
	ErrNotImported        = errors.New("typewirego: wire is not an import of this creator")
	ErrClosing            = errors.New("typewirego: container is closing")
	ErrTeardownTimeout    = errors.New("typewirego: teardown timed out")
	ErrCreator            = errors.New("typewirego: creator failed")
)

// ErrObserverResolve is a resolve the observer's resolver refuses:
// during NewContainer, for a transient token, a cache miss before the first
// non-observer resolve or while closing, after closed, or once the observer is
// disabled. The latter cases also match ErrNotResolved or ErrClosing.
var ErrObserverResolve = errors.New("typewirego: observer resolver refused")

// CircularDependencyError is a resolve that would wait on itself. Path holds
// the token labels from the root of the resolve chain to the repeated token.
type CircularDependencyError struct {
	Path []string
}

func (e *CircularDependencyError) Error() string {
	return ErrCircularDependency.Error() + ": " + strings.Join(e.Path, " -> ")
}

// Is matches ErrCircularDependency.
func (e *CircularDependencyError) Is(target error) bool {
	return target == ErrCircularDependency
}

// CreatorError is a creator that returned an error or panicked, including
// a creator whose import failed. Cause is reachable through errors.Is and
// errors.As.
type CreatorError struct {
	Label string
	Cause error
}

func (e *CreatorError) Error() string {
	return fmt.Sprintf("typewirego: creator %s failed: %v", e.Label, e.Cause)
}

// Is matches ErrCreator.
func (e *CreatorError) Is(target error) bool {
	return target == ErrCreator
}

// Unwrap returns Cause.
func (e *CreatorError) Unwrap() error {
	return e.Cause
}

// TeardownTimeoutError is returned when Teardown's context ends while
// creations or cleanups are still running, or when the observer's drainer
// is abandoned inside OnEvent or its Flush overruns its allowance while
// there is an observer set to clean. Running lists the creations' labels;
// Cleanups lists the values whose cleanup call was still running when Teardown
// stopped waiting for it; HeldBack lists the values the running creations,
// cleanups and observer calls import, which Teardown leaves until they finish.
type TeardownTimeoutError struct {
	Running  []string
	HeldBack []string
	Cleanups []string
}

func (e *TeardownTimeoutError) Error() string {
	var b strings.Builder
	b.WriteString(ErrTeardownTimeout.Error())
	part := func(name string, labels []string) {
		if len(labels) > 0 {
			fmt.Fprintf(&b, "; %s: %s", name, strings.Join(labels, ", "))
		}
	}
	part("running", e.Running)
	part("cleanups", e.Cleanups)
	part("held back", e.HeldBack)
	return b.String()
}

// Is matches ErrTeardownTimeout.
func (e *TeardownTimeoutError) Is(target error) bool {
	return target == ErrTeardownTimeout
}

// panicError is a recovered panic, kept as an error with its value.
type panicError struct {
	value any
}

func (e *panicError) Error() string { return fmt.Sprintf("typewirego: panic: %v", e.value) }

// errGoexit is the cause of a creator that called runtime.Goexit.
var errGoexit = errors.New("typewirego: creator called runtime.Goexit")

func notRegistered(t Token) error {
	return fmt.Errorf("%w: %s", ErrWireNotRegistered, t.Label())
}

func closingErr(t Token) error {
	return fmt.Errorf("%w: %s", ErrClosing, t.Label())
}
