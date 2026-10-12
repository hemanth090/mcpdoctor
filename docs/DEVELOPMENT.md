# Development

## Setup

```sh
git clone https://github.com/mcpdoctor/mcpdoctor
cd mcpdoctor
make build
```

Go 1.25 or newer. The `toolchain` line in `go.mod` pins the patch release, which matters:
the Go standard library carries its own advisories, and `go 1.25.0` alone will happily
build with a toolchain that has known `net/http` CVEs. Pinning `go1.27.2` cleared nine
of them.

## Before you push

```sh
make verify
```

Formatting, vet, tests, race detector, govulncheck, and cross-compilation. This is the
same set CI runs, minus the multi-OS matrix.

`make help` lists everything.

## Layout

```
cmd/mcpdoctor/        Entry point. No logic beyond wiring and os.Exit.
cmd/mockserver/       Test-only MCP server, built on the official SDK.

internal/diagnose/    Finding model, codes, severities, exit codes.  Leaf module.
internal/redact/      Secret redaction and control-character stripping.  Leaf module.
internal/config/      Discovery, positioned JSON parsing, client adapters, rules.
internal/protocol/    Process lifecycle, transports, the consented probe.
internal/render/      Human and JSON output.  Pure functions over a report.
internal/cli/         Flags, consent, wiring, exit codes.
internal/testutil/    Mock server construction for tests.

docs/                 Specification, architecture, threat model, compatibility.
.github/workflows/    CI and release.
```

### Dependency rules

```
cli ──▶ diagnose ◀── config
 │                    ▲
 ├──▶ protocol ───────┘
 └──▶ render ──▶ redact
```

- `diagnose` and `redact` are leaves. They import nothing internal, which is what makes
  the code contract and the redaction sink testable without a server or a terminal.
- `config` and `protocol` never import `cli` or `render`. Diagnostic logic stays
  independent of presentation.
- No package-level mutable state. Exit codes live on a `run` value, which is why two
  invocations in one process cannot interfere.
- `context.Context` is the first parameter on anything that can block or spawn.

Adding a dependency requires discussion in a pull request. The current direct dependency
count is two: the MCP SDK and cobra.

## Adding a client adapter

1. Add a `Client` constant in `internal/config/adapters.go`.
2. Add an `Adapter` entry with the documented paths, root key, URL keys, and accepted
   `type` values.
3. Add a test in `internal/config/validate_test.go` asserting the root key and, if the
   format has a quirk, a test that the quirk is handled.

If the client uses YAML or TOML, set the format accordingly — MCP Doctor will detect and
report it rather than parsing it. Do not add a YAML parser to work around this.

Cite the vendor documentation in the adapter's comment. An undocumented path is a guess,
and a guess that ages badly is worse than an honest gap.

## Adding a diagnostic code

1. Add the constant in `internal/diagnose/codes.go` with a comment explaining when it
   fires and why the severity is what it is.
2. Add it to the appropriate `configCodes`, `serverCodes`, or `transportCodes` list.
3. Add a test asserting the code, the severity, and that a remedy is present.

Codes are never reused and never renumbered. A user may have automation matching on one;
changing its meaning silently is worse than the diagnostic being wrong.

## Adding a validation rule

1. Write the rule in `internal/config/validate.go` as `func(a Adapter, s Server, r *redact.Redactor) []diagnose.Finding`.
2. Register it in `rules` — order matters, so place it after the rules it depends on.
3. Add tests for the positive case and for each way it can misfire.

If the rule touches an env value, header, or argument, register it with the redactor
before it can be printed. `LooksSecret` decides whether a value is a credential; use it
rather than registering everything, because redacting ordinary words in prose teaches
users to ignore redaction.

## Testing conventions

- Test behaviour, not implementation. Assert on state, never on which methods were called.
- `DAMP` over `DRY` in tests: each test should read as a complete specification.
- Name tests as specifications: `TestProbeServerThatHangsIsDiagnosedAsTimeout`, not
  `TestProbeTimeout`.
- Prefer a real implementation to a mock. The mock server is the SDK's real server.
- For a bug fix, write the failing test first and confirm it fails.

## Building releases

```sh
git tag -a v1.0.0 -m "v1.0.0"
git push origin v1.0.0
```

The release workflow builds all six platform targets with `-trimpath` and fixed
`ldflags`, which makes two builds of the same commit byte-identical (verified by
sha256). It publishes checksums and, when `COSIGN_PRIVATE_KEY` is configured, signs them.

The workflow refuses to release if the working tree is dirty, because a reproducible
build is impossible against uncommitted state.

## Project conventions

- Standard library first.
- Explicit over clever. Three similar lines beat a premature abstraction.
- Comments explain *why*, not *what*. If a comment restates the code, delete it.
- Errors carry a stable diagnostic code so callers never match on message text.
- No `panic` except for programmer error; every failure path returns an error.
- Prefer explicit behaviour over magic. A flag that can only lower a limit, never raise
  it, is deliberate.