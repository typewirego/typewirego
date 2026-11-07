# typewirego

[![CI](https://github.com/typewirego/typewirego/actions/workflows/ci.yml/badge.svg)](https://github.com/typewirego/typewirego/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/typewirego/typewirego.svg)](https://pkg.go.dev/github.com/typewirego/typewirego)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Lightweight, container-agnostic dependency injection for Go — typed, explicit, composable.

typewirego is the Go port of TypeWire. A wire is an immutable, package-level
description of how to build a value; a container binds wires late, builds values
lazily, caches singletons, detects cycles and tears everything down in reverse
creation order.

## Installation

```bash
go get github.com/typewirego/typewirego
```

Requires Go 1.25 or newer. Standard library only.

```go
import "github.com/typewirego/typewirego"
```

## Example

```go
type Config struct{ DSN string }
type Store struct{ DSN string }

// Wires are package-level values in a real service.
configWire := typewirego.Of(typewirego.Opts[*Config]{
	Token: "app.Config",
	Creator: func(context.Context) (*Config, error) {
		return &Config{DSN: "postgres://localhost/app"}, nil
	},
})
storeWire := typewirego.Of(typewirego.Opts[*Store]{
	Token:   "app.Store",
	Imports: []typewirego.Import{{Name: "config", Wire: configWire}},
	CreateWith: func(_ context.Context, deps typewirego.Deps, _ typewirego.Resolver) (*Store, error) {
		cfg, err := typewirego.Dep(deps, configWire)
		if err != nil {
			return nil, err
		}
		return &Store{DSN: cfg.DSN}, nil
	},
})

ctx := context.Background()
c := typewirego.NewContainer(ctx)
defer c.Teardown(ctx)

// Apply binds the store and, first, its import.
if err := storeWire.Apply(c); err != nil {
	panic(err)
}
store, err := storeWire.GetInstance(ctx, c)
if err != nil {
	panic(err)
}
again, _ := storeWire.GetInstance(ctx, c)
fmt.Println(store.DSN)
fmt.Println(store == again)
```

## Features

- **Late binding** — the last binding for a token wins, so tests override a wire with
  `WithCreator` without touching the code that imports it.
- **Lazy singletons** — nothing is built until the first resolve; singletons are built
  once per container and cached, zero values included. Transients are built on every
  resolve.
- **Cycle detection** — a runtime cycle fails with the token path instead of hanging,
  across goroutines too.
- **Ordered teardown with joined errors** — values are cleaned through their own
  `Shutdown(ctx) error`, `Close() error` or `Close()`, in reverse creation order, once
  each, and every failure is returned joined. `KeepOpen` protects borrowed values.
- **Concurrent resolution** — concurrent first resolves share one creation; a caller's
  context bounds only its own wait; singleton creators run under a container context
  that Teardown cancels, and `Lifetime(deps)` outlives it.
- **Bounded shutdown** — a Teardown that times out still runs the whole path, holds back
  values still in use, returns `*TeardownTimeoutError`, and `Wait` reports the rest.
- **Observer** — an observe-only hook for lifecycle events, delivered in order and never
  blocking the container.
- **Graph export** — `Container.Graph` renders the wiring as DOT or JSON.

## Documentation

- [Package documentation](https://pkg.go.dev/github.com/typewirego/typewirego) on
  pkg.go.dev — the normative contract, with runnable examples.
- [Specification](docs/SPEC.md) — rules P1–P20 in prose, each linked to the tests that
  pin it.
- [Best practices](docs/best-practices.md) — how to wire a Go service.
- [Contributing](CONTRIBUTING.md) and [maintaining](MAINTAINING.md).

## Other ports

TypeWire has one design and three ports. Rules P1–P10 (late binding, lazy creation,
singletons, transients, retry, rebinding, cycles, teardown order, single use and joined
cleanup errors) are shared, and a common conformance suite holds every port to them.
Rules P11–P20 (concurrency, contexts and the observer) are Go-only today.

- Python: [typewirepy](https://github.com/typewirepy/typewirepy) (latest release
  [v0.2.0](https://github.com/typewirepy/typewirepy/releases/tag/v0.2.0))
- TypeScript: [typewirets](https://github.com/typewirets/typewirets) (latest release
  [v0.1.0](https://github.com/typewirets/typewirets/releases/tag/v0.1.0))

## License

MIT, see [LICENSE](LICENSE).
