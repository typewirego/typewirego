# Maintaining typewirego

Policies for maintainers. Contributor commands are in [CONTRIBUTING.md](CONTRIBUTING.md),
agent and supply-chain rules in [AGENTS.md](AGENTS.md).

## Go version policy

- The `go` line in `go.mod` is the lowest Go version the module and its tests build on.
  It is `go 1.25.0`, because the tests use `sync.WaitGroup.Go`.
- Raise it only when the code needs a newer feature, and say so in
  [CHANGELOG.md](CHANGELOG.md).
- CI tests that version plus the two releases Go currently supports: today 1.25.x,
  1.26.x and 1.27.x. Update the matrix in `.github/workflows/ci.yml` when Go releases.
- There is no `toolchain` line: a library should not choose its users' toolchain.

## No dependencies

The module uses the standard library only. `go.mod` has no `require` block and keeps
none, and `go mod tidy -diff` in CI must print nothing. Tools never enter `go.mod`
(no `tool` directives either); CI and the `justfile` run them with
`go run module@exact-version`:

| Tool | Module | Pinned version |
|---|---|---|
| staticcheck | `honnef.co/go/tools/cmd/staticcheck` | `v0.8.2` |
| govulncheck | `golang.org/x/vuln/cmd/govulncheck` | `v1.9.0` |

Update both places (`ci.yml` and `justfile`) together, in a PR of their own.

## Conformance fixtures

`testdata/conformance/cases.json` and `native_tests.json` are a pinned copy of the
TypeWire conformance suite. To update them:

1. Replace each file whole with the new copy, in a commit of its own.
2. Update the SHA-256 checksums in
   [testdata/conformance/README.md](testdata/conformance/README.md) in the same commit,
   and check them with `shasum -a 256 testdata/conformance/*.json`.
3. If the case count changes, update `expectedGoCases` in `conformance_test.go`; if the
   schema adds an op, teach the driver in a separate commit.
4. If `native_tests.json` names a new test, add the test in the same PR.

## Releases

Go has no registry: a release is an annotated semver tag on `main`, which the module
proxy and pkg.go.dev serve.

1. Add a `## <version>` section to [CHANGELOG.md](CHANGELOG.md) and merge it.
2. Run **Actions → Release → Run workflow** with `version` (for example `0.1.0`) and
   `dry-run` left on. It validates the version, checks the tag is new, runs CI and checks
   the CHANGELOG heading.
3. Run it again with `dry-run` off. It creates and pushes tag `v<version>`, creates the
   GitHub release with generated notes, and asks the Go module proxy for the version so
   pkg.go.dev picks it up.
4. Check `go get github.com/typewirego/typewirego@v<version>` from a clean module, and
   the package page on pkg.go.dev.

Rules that follow from Go's checksum database:

- **A pushed tag is permanent.** Never move or delete one; the checksum database has
  recorded it.
- **A bad release is withdrawn with `retract`** in `go.mod`, shipped in the next
  release.
- **Stay on v0.** `v2` and later would need a `/v2` module path.

Agents never push tags.

## Security-sensitive changes

Every change under `.github/` is security-sensitive and says so in its PR description.
Actions are pinned to full commit SHAs looked up in the action's own repository, Go tools
to exact versions; see [AGENTS.md](AGENTS.md#security-supply-chain-and-ai-driven-attacks).
The repository settings this relies on:

- branch protection on `main` requires the `CI OK` check;
- Actions default to a read-only `GITHUB_TOKEN`; only the release job asks for
  `contents: write`;
- Dependabot alerts are on, and Dependabot updates actions and Go modules weekly.
