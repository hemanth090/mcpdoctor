# Spec: MCP Doctor v1.0.0

## Objective

MCP Doctor is a single-binary Go CLI that diagnoses Model Context Protocol (MCP) server
configurations and running servers. It answers two questions that developers currently
answer by guesswork:

1. **Is my configuration correct?** — static validation of client config files.
2. **Will this server actually start and speak MCP?** — live protocol probe.

Success looks like: a developer hits `mcp-doctor check`, gets a specific finding with a
stable code and a copyable fix, and CI can gate on the exit code.

**Primary users:** developers debugging a local MCP setup; CI pipelines validating a
committed `.mcp.json`; teams standardising on MCP across a fleet of machines.

**Non-goals for v1:** editing/writing user config, installing servers, auto-remediation,
a TUI, telemetry, a server registry, proxying MCP traffic.

## Assumptions (surfaced per spec-driven-development)

These were resolved from authoritative sources unless marked otherwise. A reviewer who
disagrees with any of these should say so before the release ships.

| # | Assumption | Basis |
|---|---|---|
| 1 | Module path is `github.com/mcpdoctor/mcpdoctor`; the project is not yet published, so this is a placeholder to rename at publish time. | — |
| 2 | Target MCP protocol revision is **2026-07-28**, with graceful negotiation back to 2024-11-05. | Official Go SDK `v1.8.0` `mcp.SupportedProtocolVersions()` |
| 3 | We depend on the **official Go SDK** (`github.com/modelcontextprotocol/go-sdk`) rather than hand-rolling JSON-RPC. It is v1.x (GA). | `proxy.golang.org` module tags |
| 4 | Supported transports for live probing: **stdio** and **Streamable HTTP**. Legacy HTTP+SSE is out of scope for v1 but detected statically. | MCP transports spec |
| 5 | Config formats follow each vendor's *documented* `mcpServers` JSON shape; unknown keys are warnings, never hard errors, because clients add keys over time. | Client config research |
| 6 | **Execution requires explicit opt-in.** Static `check` never executes. `probe` requires `--execute`, plus a terminal confirmation unless `--yes` is passed. | Safety requirement |
| 7 | We do **not** modify or repair user configuration, ever. | Product requirement |
| 8 | `server/discover` (a newer SDK capability) is used opportunistically if the server supports it, with fallback to `initialize`. | SDK `mrtr.go` |
| 9 | No network egress other than the Streamable HTTP endpoint the user explicitly configured. No telemetry. | Product requirement |
| 10 | Exit codes: `0` healthy, `1` findings at/above threshold, `2` operational/usage failure, `130` cancelled. | Product requirement |

## Tech Stack

| Component | Choice | Version |
|---|---|---|
| Language | Go | 1.27.1 (module targets `go 1.24`) |
| MCP protocol | `github.com/modelcontextprotocol/go-sdk` | `v1.8.0` |
| CLI framework | `github.com/spf13/cobra` | latest stable |
| Module resolution | `github.com/google/jsonschema-go` (indirect, via SDK) | — |
| Tests | stdlib `testing`, `net/http/httptest` | — |
| Lint | `go vet`, `staticcheck`, `golangci-lint` in CI | — |
| Module path | `github.com/mcpdoctor/mcpdoctor` | — |

No database, no service, no container, no cloud dependency.

## Commands

```
Build:              go build -o bin/mcpdoctor ./cmd/mcpdoctor
Install:            go install github.com/mcpdoctor/mcpdoctor/cmd/mcpdoctor@latest
Test:               go test ./...
Race:               go test -race ./...
Vet:                go vet ./...
Format check:       test -z "$(gofmt -l .)"
Staticcheck:        staticcheck ./...
Fuzz smoke:         go test -run=XXX -fuzz=FuzzParseServerConfig -fuzztime=30s ./internal/config
Coverage:           go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out
Cross-compile:      go build for darwin/linux/windows on amd64+arm64
```

## Project Structure

```
cmd/mcpdoctor/          Entry point. Parses nothing; calls cli.Main and exits.
internal/cli/           Cobra commands, flag validation, exit-code mapping.
internal/config/        Discovery, JSON parsing with line/col, client adapters.
internal/diagnose/      Finding model, severity, diagnostic codes, rule engine.
internal/protocol/      Live MCP probe: process lifecycle, stdio + HTTP transports.
internal/render/        Human and JSON output renderers; versioned report schema.
internal/redact/        Secret redaction used by every output path.
internal/testutil/      Mock MCP servers + fixture helpers for tests.
docs/                   SPEC, ARCHITECTURE, THREAT-MODEL, COMPATIBILITY, etc.
.github/workflows/      CI: build, test, race, fuzz smoke, govulncheck, release.
```

### Dependency direction (enforced, no cycles)

```
cli ──▶ diagnose ──▶ (nothing)
 │       ▲
 ├──▶ config ───────┘
 ├──▶ protocol ─────┘
 └──▶ render ───────┘
        │
     redact (leaf, imported by diagnose + render + protocol)
```

`config` and `protocol` never import `cli` or `render`. `diagnose` is a leaf that both
feed. This keeps diagnostic logic independent of CLI rendering, per the architecture
requirement.

## Code Style

Standard library first. Errors are wrapped with `%w` and carry a stable
`diag.Code`. Finding codes are strings, never integers, so they never renumber.

```go
// Finding codes are the tool's public contract. Never renumber or reuse one.
const (
    CodeCommandNotFound = "MDC1009"
    CodeServerTimeout   = "MDC2003"
)

type Finding struct {
    Code       string
    Severity   Severity
    Target     string        // server name, or config path
    Location   string        // "path/to/file.json:12" when known
    Message    string
    Detail     string
    Remedy     string
    Example    string        // copyable snippet, already redacted
}
```

Conventions:
- `context.Context` is the first parameter on anything that can block or spawn.
- No global mutable state. No package-level `var` that changes after `init`.
- Constructors take explicit options structs, not a bag of positional flags.
- Exported identifiers carry doc comments that state the contract, not the mechanics.
- Errors are returned, not panicked. `panic` is reserved for programmer error.

## Testing Strategy

| Level | What | Where |
|---|---|---|
| Unit | Parsers, validators, rules, redaction, exit codes | `_test.go` beside code |
| Integration | Mock stdio + HTTP MCP servers, real process spawn | `internal/protocol` |
| Conformance | Version negotiation, capability checks, `tools/list` validation | `internal/protocol` |
| Security | Malicious paths, hostile output, oversized messages, secret leakage | `internal/*_security_test.go` |
| Fuzz | Config parser, JSON-RPC line decoder | `Fuzz*` funcs |
| E2E | Whole CLI against fixture configs and mock servers | `internal/cli` |

**Coverage target:** ≥ 80% statement coverage on `internal/config`, `internal/diagnose`,
`internal/redact`, and `internal/render`. `internal/protocol` is process-heavy, so the
target there is behavioural (every failure path exercised) rather than a percentage.
Coverage is a floor, not a goal — a test that asserts nothing is worse than no test.

## Boundaries

**Always:**
- `go test ./...` passes before any commit.
- Every new finding code gets a unit test asserting code, severity, and remedy.
- Every untrusted input is bounded before parsing.
- Every error path returns a non-generic message.

**Ask first:**
- Adding a runtime dependency.
- Adding a new MCP client adapter or transport (widens the support surface).
- Changing exit codes or the JSON schema major version.

**Never:**
- Execute a configured command without explicit user authorisation.
- Modify, reformat, or "fix" a user configuration file.
- Send telemetry or any network request other than the probed endpoint.
- Put a real credential in source, testdata, or a golden file.
- Commit a test whose assertion was weakened to make it pass.

## Success Criteria

1. `mcpdoctor check` discovers and validates every documented client config on the
   machine, and on `--config <path>` for a specified file.
2. Every finding has code, severity, target, location, message, and remedy.
3. `mcpdoctor probe --execute --yes` starts a server, completes initialization, and
   reports negotiated version, capabilities, and validated `tools/list`.
4. `mcpdoctor probe` without `--execute` runs zero subprocesses (asserted by a test).
5. Secrets from `env` and `headers` never appear in any output path (asserted by tests
   over human, JSON, and error renderers).
6. Exit codes match the documented table exactly.
7. `go test ./...`, `go test -race ./...`, `go vet ./...`, `gofmt -l`, `staticcheck`,
   `govulncheck`, and cross-compilation all pass in CI.
8. Documentation claims match tested behaviour; every unsupported case is documented
   as a limitation rather than implied to work.

## Open Questions

- None blocking. See `docs/COMPATIBILITY.md` for the resolved compatibility decisions
  and their evidence.