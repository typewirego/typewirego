# Changelog

All notable changes to typewirego are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[semantic versioning](https://semver.org/) (pre-1.0: minor versions may break).

## 0.1.0

Initial public release.

- Wires (`Of`, `TypeWire`, `Opts`, `Import`, `Dep`), groups (`GroupOf`,
  `CombineGroups`) and test overrides (`WithCreator`), with the same semantics as
  typewirepy and typewirets: late binding, lazy creation, a singleton cache, transients,
  retry after failure and runtime cycle detection (P1–P7).
- `Container` with ordered teardown: cleanup by the value's own `Shutdown(ctx) error`,
  `Close() error` or `Close()`, in reverse creation order, once per value, with every
  failure joined (P8–P10). `KeepOpen` for borrowed values.
- Concurrent resolution through a per-token latch, with cross-goroutine cycle detection;
  singleton creators run under a container context and `Lifetime(deps)` outlives Closing
  (P11, P13, P18).
- Bounded teardown: in-flight creations are cancelled and waited for, and a timeout still
  runs the whole path, holds back values still in use and returns
  `*TeardownTimeoutError`; `Wait` reports the late results (P12, P19).
- An observe-only `Observer` with ordered, non-blocking event delivery, an enforced
  Resolver, an observer set cleaned last, and a fixed Flush budget (P14–P17, P20).
- Graph export (`Container.Graph`) with DOT and JSON renderings.
- Requires Go 1.25 or newer. Standard library only.
