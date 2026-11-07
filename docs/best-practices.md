# Best practices

Recommended conventions for wiring a Go service with typewirego. The library
does not enforce them; they keep a dependency graph readable, testable and
safe to shut down. Each section links to the rule in [SPEC.md](SPEC.md) it
relies on.

Every snippet below is a complete Go file, so it compiles as written.

## 1. Declare wires at package level, with fixed tokens

A wire is an immutable description of how to build a value, built once at
init. Declare it as an exported package-level variable, next to the type it
builds, so other packages can import it as a dependency, override it in tests,
and find it in the graph.

```go
package store

import (
	"context"
	"database/sql"

	"github.com/typewirego/typewirego"
)

// DBWire is the service's database handle.
var DBWire = typewirego.Of(typewirego.Opts[*sql.DB]{
	Token: "store.DB",
	Creator: func(ctx context.Context) (*sql.DB, error) {
		return sql.Open("postgres", "postgres://localhost/app")
	},
})
```

- Give every wire a fixed, readable token label, conventionally
  `package.Type`. Labels are diagnostic only (tokens compare by identity, see
  [P1](SPEC.md#p1-late-binding)), but they are what errors, events and the
  graph show you.
- `Of` panics on invalid options, which is what you want for package-level
  values: the mistake surfaces at init, not at the first request.
- If nothing outside a package needs to reference, override or clean up a
  dependency, it does not need a wire: build it inside the creator that uses
  it.

## 2. `Creator` for leaf wires, `CreateWith` and `Dep` for wires with imports

A wire with no dependencies uses `Creator`. A wire with dependencies declares
them in `Imports` and builds its value in `CreateWith`, reading each import
with `Dep`. Imports resolve in declaration order, before the creator runs
([P1](SPEC.md#p1-late-binding), [P5](SPEC.md#p5-retry-after-failure)).

```go
package users

import (
	"context"

	"github.com/typewirego/typewirego"
)

type Config struct{ Table string }

type Repo struct{ table string }

var ConfigWire = typewirego.Of(typewirego.Opts[*Config]{
	Token:   "users.Config",
	Creator: func(context.Context) (*Config, error) { return &Config{Table: "users"}, nil },
})

var RepoWire = typewirego.Of(typewirego.Opts[*Repo]{
	Token:   "users.Repo",
	Imports: []typewirego.Import{{Name: "config", Wire: ConfigWire}},
	CreateWith: func(_ context.Context, deps typewirego.Deps, _ typewirego.Resolver) (*Repo, error) {
		cfg, err := typewirego.Dep(deps, ConfigWire)
		if err != nil {
			return nil, err
		}
		return &Repo{table: cfg.Table}, nil
	},
})
```

Prefer declared imports to resolving through the `Resolver` argument: declared
imports appear in the graph, are registered by `Apply`, and are what `Dep`
checks. Resolve through the `Resolver` only for dependencies chosen at run
time; the resolve is still checked for cycles ([P7](SPEC.md#p7-cycles)).

## 3. Compose with groups

Collect a package's wires into a `Group`, and combine groups at the
entrypoint. A group applies its wires in order and stops at the first error.

```go
package app

import (
	"context"

	"github.com/typewirego/typewirego"
)

var cfgWire = typewirego.Of(typewirego.Opts[string]{Token: "app.Config",
	Creator: func(context.Context) (string, error) { return "cfg", nil }})

var dbWire = typewirego.Of(typewirego.Opts[string]{Token: "app.DB",
	Creator: func(context.Context) (string, error) { return "db", nil }})

var StorageGroup = typewirego.GroupOf(cfgWire, dbWire)

var HTTPGroup = typewirego.GroupOf(typewirego.Of(typewirego.Opts[string]{Token: "app.Server",
	Creator: func(context.Context) (string, error) { return "server", nil }}))

var All = typewirego.CombineGroups(StorageGroup, HTTPGroup)

func Wire(c *typewirego.Container) error { return All.Apply(c) }
```

Nothing is created when a group is applied ([P2](SPEC.md#p2-lazy-creation)):
apply everything at start-up and let the first resolve build what it needs.

## 4. Override in tests with `WithCreator`

`WithCreator` returns a wire with the same token whose value comes from your
function. The original's imports stay declared but are resolved only if you
call `original`, so a test double never dials the real dependency
([P1](SPEC.md#p1-late-binding)).

Binding is late and the last binding wins, but applying a wire skips any import
that is already bound, and rebinding never clears a cached value
([P6](SPEC.md#p6-rebinding-never-clears-a-cache)). So where the override goes
depends on what you override:

- **An import** of the wire you apply is overridden by applying the override
  *first*: the original's later Apply sees the token bound and keeps it.
- **A wire you apply directly** is overridden by applying the override *after*
  it, for example by appending it with `WithExtraWires`.
- Either way, apply every override before the first resolve.

```go
package users_test

import (
	"context"
	"testing"

	"github.com/typewirego/typewirego"
)

type Config struct{ Table string }

type Repo struct{ Table string }

var ConfigWire = typewirego.Of(typewirego.Opts[*Config]{Token: "users.Config",
	Creator: func(context.Context) (*Config, error) { return &Config{Table: "users"}, nil }})

var RepoWire = typewirego.Of(typewirego.Opts[*Repo]{Token: "users.Repo",
	Imports: []typewirego.Import{{Name: "config", Wire: ConfigWire}},
	CreateWith: func(_ context.Context, d typewirego.Deps, _ typewirego.Resolver) (*Repo, error) {
		cfg, err := typewirego.Dep(d, ConfigWire)
		if err != nil {
			return nil, err
		}
		return &Repo{Table: cfg.Table}, nil
	}})

var Group = typewirego.GroupOf(RepoWire)

func TestRepoWithAFakeConfig(t *testing.T) {
	ctx := context.Background()
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)

	// An import: apply the override first, so RepoWire's Apply keeps it.
	fakeConfig := ConfigWire.WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[*Config]) (*Config, error) {
		return &Config{Table: "users_test"}, nil
	})
	if err := typewirego.GroupOf(fakeConfig).Apply(c); err != nil {
		t.Fatal(err)
	}
	if err := Group.Apply(c); err != nil {
		t.Fatal(err)
	}
	repo, err := RepoWire.GetInstance(ctx, c)
	if err != nil || repo.Table != "users_test" {
		t.Fatalf("repo = %+v, %v", repo, err)
	}
}

func TestWithAFakeRepo(t *testing.T) {
	ctx := context.Background()
	c := typewirego.NewContainer(ctx)
	defer c.Teardown(ctx)

	// A wire the group applies directly: append the override, so it is
	// bound after the original.
	fakeRepo := RepoWire.WithCreator(func(context.Context, typewirego.Deps, typewirego.Resolver, typewirego.Creator[*Repo]) (*Repo, error) {
		return &Repo{Table: "fake"}, nil
	})
	if err := Group.WithExtraWires(fakeRepo).Apply(c); err != nil {
		t.Fatal(err)
	}
	repo, _ := RepoWire.GetInstance(ctx, c)
	if repo.Table != "fake" {
		t.Fatalf("repo = %+v", repo)
	}
}
```

A wrapping override can call `original(ctx, deps, r)` to build the real value
and decorate it.

## 5. `KeepOpen` for borrowed values

Teardown closes every singleton value that has a closer
([P8](SPEC.md#p8-teardown-order-and-cleanup-by-value)): `Shutdown(ctx) error`,
`Close() error` or `Close()`. A wire that hands out a value it does not own,
such as `os.Stdout` or a client passed in from `main`, sets `KeepOpen` so the
container never closes it.

```go
package app

import (
	"context"
	"net/http"
	"os"

	"github.com/typewirego/typewirego"
)

var StdoutWire = typewirego.Of(typewirego.Opts[*os.File]{
	Token:    "app.Stdout",
	KeepOpen: true,
	Creator:  func(context.Context) (*os.File, error) { return os.Stdout, nil },
})

// ClientWire returns a client the entrypoint built and owns.
func ClientWire(client *http.Client) typewirego.TypeWire[*http.Client] {
	return typewirego.Of(typewirego.Opts[*http.Client]{
		Token:    "app.HTTPClient",
		KeepOpen: true,
		Creator:  func(context.Context) (*http.Client, error) { return client, nil },
	})
}
```

The container recognizes a value again by `==`, so a borrowed value whose
dynamic type is not comparable (a map or slice type with `Close`) must be
wrapped in a pointer, or every wire returning it must be `KeepOpen`.

Do not mutate a value you received as an import and return it with another
dependency attached: the value keeps its first creator's teardown slot, so the
attached dependency is closed before it. Wrap it in a new type instead.

## 6. `Lifetime(deps)` for work that must outlive Closing

The `ctx` a singleton creator receives is cancelled the moment Teardown begins
([P13](SPEC.md#p13-container-context-for-singleton-creators),
[P18](SPEC.md#p18-two-contexts)). Use it to abort slow construction. A value
that keeps background work running until its own cleanup, such as a batching
flusher, takes `Lifetime(deps)`, which ends only after that value's own
cleanup slot.

```go
package metrics

import (
	"context"
	"time"

	"github.com/typewirego/typewirego"
)

type Flusher struct{ done chan struct{} }

func (f *Flusher) Shutdown(ctx context.Context) error {
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var FlusherWire = typewirego.Of(typewirego.Opts[*Flusher]{
	Token: "metrics.Flusher",
	CreateWith: func(_ context.Context, deps typewirego.Deps, _ typewirego.Resolver) (*Flusher, error) {
		life := typewirego.Lifetime(deps)
		f := &Flusher{done: make(chan struct{})}
		go func() {
			defer close(f.done)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					// flush a batch
				case <-life.Done():
					return
				}
			}
		}()
		return f, nil
	},
})
```

## 7. Always tear down, and handle timeouts

Every container must be torn down; a container with an observer must be,
because its drainer goroutine exits only in Teardown
([P14](SPEC.md#p14-the-observer-observes-only)). Give Teardown a deadline, and
treat `*TeardownTimeoutError` as "shutdown is still finishing": the values
still in use are cleaned once the running creations and calls return, and
`Wait` reports those errors ([P12](SPEC.md#p12-closing-waits-for-in-flight-creations),
[P19](SPEC.md#p19-timeouts-still-run-the-whole-path)).

```go
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/typewirego/typewirego"
)

var serverWire = typewirego.Of(typewirego.Opts[string]{Token: "app.Server",
	Creator: func(context.Context) (string, error) { return "server", nil }})

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Cancelling ctx when the drain begins fails no creation: NewContainer
	// does not retain it.
	c := typewirego.NewContainer(ctx)
	if err := serverWire.Apply(c); err != nil {
		log.Fatal(err)
	}
	if _, err := serverWire.GetInstance(ctx, c); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()

	tctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Teardown(tctx)
	var timeout *typewirego.TeardownTimeoutError
	if errors.As(err, &timeout) {
		log.Printf("teardown still running: %v; held back: %v", timeout.Running, timeout.HeldBack)
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		if werr := c.Wait(wctx); werr != nil {
			log.Printf("late cleanup: %v", werr)
		}
	} else if err != nil {
		log.Printf("cleanup errors: %v", err)
	}
}
```

Teardown returns every cleanup failure joined
([P10](SPEC.md#p10-every-cleanup-runs-every-failure-is-returned)); match them
with `errors.Is` and `errors.As`, never by message text
([Errors](SPEC.md#errors)).
