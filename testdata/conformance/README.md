# Conformance fixtures

These files hold typewirego to the TypeWire contract.

| File | What it is | SHA-256 |
|---|---|---|
| `cases.json` | The shared, language-neutral TypeWire conformance suite for rules P1–P10. The Python and TypeScript ports run the same cases. | `98b16254d967006791b01836df1b64dae61387a5e51cd9c2fd88cd89eb2f2c2e` |
| `native_tests.json` | A map from each Go-only rule, P11–P20, to the exact Go test names that pin it. | `1774071537118d895f13c8973cb5674280d67f1ee221eee2fa25159a57c53de8` |

`TestConformanceCases` runs every Go-applicable case in `cases.json` (34 of the
35) and fails unless exactly 34 ran. `TestNativeTestsManifestNamesExist` fails
if a test named in `native_tests.json` is not defined. Set
`TYPEWIRE_CONFORMANCE_CASES` to the path of another `cases.json` to run a
different copy; `native_tests.json` is then read from the same directory.

## Provenance and updates

`cases.json` is the shared TypeWire suite. The copy of record for this
repository is the one written when the repository was bootstrapped, with the
checksums above.

- **Never edit these files here.** Not to fix a typo, not to reword a label
  that does not match this repository's headings (see the glossary below).
- **To update**, replace a whole file with the new copy of the shared suite, in
  a commit of its own, and update the checksums in this table in the same
  commit. Verify them with `shasum -a 256 testdata/conformance/*.json` (or
  `sha256sum`). If the new copy adds a case or an op, change the driver in a
  separate commit.

## Glossary

### Section labels in `cases.json`

The suite's `description` cites section labels from the design document it was
written against. In this repository they map to:

| Label | Here |
|---|---|
| A1 (the rules) | [`docs/SPEC.md`](../../docs/SPEC.md) `## P1` to `## P20` |
| A2 (graph export) | [`docs/SPEC.md`](../../docs/SPEC.md) `## Graph export` |
| A4 (the case schema) | [Case schema](#case-schema), below |
| A5 (the observer) | [`docs/SPEC.md`](../../docs/SPEC.md) `## P14` to `## P20` |

### Rationale codes

A case with a `languages` list runs only on those ports, and must give one of
the `rationales` as its reason.

| Code | Meaning | Used here |
|---|---|---|
| `introspection` | The case needs a language feature only some ports use. The one case using it is Python's one-argument override, which inspects the function. | 1 case (Python only) |
| `capability-absent` | A port lacks the capability under test: TypeScript has no teardown or graph export yet, Python has no graph export. | 10 cases |
| `python-narrowing-deferred` | Reserved: declared in the shared list for other fixture files. | none |
| `q9-declined` | Reserved: declared in the shared list for other fixture files. | none |
| `reference-bug` | Reserved: declared in the shared list for other fixture files. | none |

## Case schema

The top-level `ops` object of `cases.json` is the normative schema; this is a
summary of it.

- **File.** `description`, `languages` (the ports), `rationales` (the permitted
  rationale codes), `ops` (the schema) and `cases`.
- **Case.** `name`, `rules` (rule IDs), `operations`, and optionally
  `languages` with a `rationale`. A case is Go-applicable when `languages` is
  absent or contains `"go"`.
- **State.** Each case owns one fresh container. Drivers count creator calls
  per id (an override's creator under `<id>.override`) and log cleanups in the
  order they run.

| Op | Keys | Meaning |
|---|---|---|
| `define` | `id`, `label`, `creator`; optional `scope`, `imports`, `resolves`, `value`, `cleanup` | A wire with a new token. `id` matches `^[a-z][a-z0-9_]*$` and is defined once; `imports` name ids defined earlier; `resolves` are resolved through the creator's Resolver after its imports. `creator` is `ok` (a fresh value compared by identity, whose cleanup logs the id), `fail`, or `value` (returns `value`, no cleanup). `cleanup` is `ok` or `fail` (logs, then fails). |
| `override` | `id`, `keep_imports` | Replace the wire for `id` with `WithCreator` of it; the override's creator never calls the original. `keep_imports: false` is Python-only. |
| `apply` | `ids` | Apply the current wires for `ids`, in order, as one group. |
| `resolve` | `id`; optional `expect_calls`, `expect_same_as`, `expect_error`, `expect_path` | Resolve `id`. Without `expect_error` it must succeed. `expect_calls` counts the original creator; `expect_same_as` compares by identity with the last value resolved for that id; `expect_path` is the cycle's label path. |
| `teardown` | optional `expect_error` (only `cleanup`), `expect_failed` | Tear the container down. Without `expect_error` it must return nil. |
| `creator_calls` | `expect` | Compare the listed creator-call counts. |
| `cleanup_order` | `expect` | Compare the full cleanup log. |
| `graph` | `expect_nodes`, `expect_edges` | Compare the graph's nodes `{id, label, scope, created, overridden}` and edges `{from, to}` as sets, mapping node ids to case ids by token. |

`expect_error` classes, matched with `errors.Is` through `*CreatorError` and
`errors.Join`: `cycle`, `not_registered`, `creator`, `closing`, and `cleanup`
(a teardown that returns cleanup errors).

A driver fails on an unknown op, an unknown key in an operation or case, or
`languages` without a permitted rationale. The Go driver also fails on an id
defined twice or used before it is defined.
