# Contributing to typewirego

## Prerequisites

- Go 1.25 or newer, or
- [Nix](https://nixos.org/download) with flakes: `nix develop --no-pure-eval` (or
  `direnv allow`, with [nix-direnv](https://github.com/nix-community/nix-direnv)) gives
  you Go, `just`, `gh` and `git`. The dev shell is built with devenv, which needs
  `--no-pure-eval` to find the project directory.

The module has no dependencies. The linters run through `go run` at pinned versions, so
the first run downloads them.

## Development setup

```bash
git clone https://github.com/typewirego/typewirego.git
cd typewirego
```

## The full check

Run this before every commit; CI runs the same steps:

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...
```

With `just` (in the Nix dev shell, or `nix develop --no-pure-eval -c just check`):

```bash
just check   # fmt check, vet, staticcheck, tests under -race, govulncheck
just test    # go test -race -count=1 ./...
just lint    # gofmt check, go vet, staticcheck
just fmt     # gofmt -w .
just vuln    # govulncheck
```

## Tests

- `TestConformanceCases` runs the shared TypeWire suite in
  [`testdata/conformance/cases.json`](testdata/conformance/cases.json). Never edit the
  fixture files; see [their README](testdata/conformance/README.md).
- The tests named in `testdata/conformance/native_tests.json` pin the Go-only rules
  P11–P20. Never rename them.
- Tests always run under the race detector. Force interleavings with channels, gates and
  contexts; never order goroutines with sleeps. A flaky test is fixed by fixing its
  synchronisation, never by raising a timeout or skipping it.

## Pull request guidelines

1. Branch from `main`.
2. One change per PR.
3. CI must be green (the `CI OK` check).
4. Use [conventional commit](https://www.conventionalcommits.org/) subjects (`feat:`,
   `fix:`, `docs:`, `test:`, `ci:`, `chore:` …): the release notes are generated from
   them.
5. A change to behaviour changes [`doc.go`](doc.go), [`docs/SPEC.md`](docs/SPEC.md) and
   the tests that pin it, in the same PR.
6. A change under `.github/` is security-sensitive: say so in the PR description. See
   [AGENTS.md](AGENTS.md) for the supply-chain rules.
7. Follow the [best practices](docs/best-practices.md) in examples and docs.
