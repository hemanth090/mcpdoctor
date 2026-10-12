# Testing strategy

MCP Doctor is a diagnostic tool. A diagnostic tool that produces wrong diagnoses is
worse than none, because it sends users chasing problems that are not there. Testing is
therefore treated as a correctness argument, not as coverage theatre.

## Commands

```sh
make test    # go test ./... -count=1
make race    # go test -race ./... -count=1
make cover   # coverage profile and totals
make fuzz    # 30s per fuzz target
make vuln    # govulncheck
make verify  # all of the above, plus fmt, vet, cross-compile
```

## Test levels

### Unit

Parsers, validation rules, redaction, exit codes, and renderers. Fast, no I/O.

The rules that matter most here assert *behaviour*, not implementation. For example, a
redaction test asserts that a secret does not appear in output — not that a particular
function was called.

### Integration

Real subprocesses speaking real MCP to the official SDK's server implementation.

The mock server is `cmd/mockserver`, a genuine SDK-backed server driven by environment
variables. This matters: testing the probe against a mock that shares its assumptions
with the probe proves nothing. A server built by the SDK exercises protocol handling we
did not write.

```sh
MOCK_TOOLS_ADVERTISED=true  MOCK_TOOL_0_NAME=read  ./mockserver
MOCK_EXIT_CODE=3            MOCK_STDERR="boom"      ./mockserver   # crashes at startup
MOCK_STARTUP_DELAY_MS=30000                           ./mockserver   # hangs
MOCK_STDOUT_NOISE="banner"                            ./mockserver   # corrupts the stream
```

### Conformance

Protocol behaviour across both transports: handshake completion, version negotiation,
capability advertisement, `tools/list`, and tool schema validation. HTTP tests run
against a real `httptest` server using the SDK's `StreamableHTTPHandler`.

### Security

Each control in [THREAT-MODEL.md](THREAT-MODEL.md) has a test.

| Control | Test |
|---|---|
| No execution without consent | `TestProbeWithoutExecuteNeverStartsAnything` — config names `touch <marker>`; asserts the marker does not exist |
| No shell interpretation | `TestCommandWithShellSyntaxIsRejected`, `TestCommandWithSemicolonIsRejected` |
| Credential isolation for the child | `TestBuildTransportEnvForwardsOnlyTheAllowlist` |
| No secret in output | `TestSecretsNeverAppearInAnyOutputPath` (human + JSON + stderr) |
| No secret in the consent prompt | `TestProbeRedactsSecretsInTheConsentPrompt` |
| No secret from server output | `TestProbeRedactsSecretsTheServerEchoes` |
| Bounded stderr | `TestStderrRingBufferKeepsTailOfChatter` |
| Bounded config reads | `TestReadFileEnforcesSizeLimit`, `TestReadBoundedRefusesExactlyLimitPlusOne` |
| Permission handling | `TestReadFileUnreadablePermissions` |
| Symlink resolution | `TestReadFileReportsSymlinkTarget` |
| Redirect bound and downgrade refusal | `TestBuildHTTPClientBoundsRedirects`, `TestBuildHTTPClientRefusesHTTPDowngradeRedirect` |
| HTTP client limits | `TestBuildHTTPClientHasExplicitTimeouts` |
| Remote endpoint consent | `TestHTTPProbeRefusesARemoteHostWithoutAllowRemote` |
| Bounded probes under hang | `TestProbeServerThatHangsIsDiagnosedAsTimeout`, `TestHTTPProbeIsBoundedWhenTheEndpointHangs` |
| Control-character stripping | `TestStripControlCharsRemovesAnsiEscapes` |

### Fuzz

Two targets, both on the JSON parser, which is the component most exposed to untrusted
bytes:

```sh
go test -run=XXX -fuzz=FuzzParseConfig       -fuzztime=30s ./internal/config/
go test -run=XXX -fuzz=FuzzScanKeyPositions -fuzztime=30s ./internal/config/
```

`FuzzParseConfig` asserts the parser never panics, always returns a *typed* error, and
never reports a position outside the input. The typed-error assertion found a real bug:
`ensureNoTrailingContent` returned a bare `*json.SyntaxError`, which would have left
callers unable to choose a diagnostic code.

`FuzzScanKeyPositions` targets the duplicate-key scanner, the part that most easily
loses track of nesting.

### Regression

Every bug fixed after the first release has a test named for the behaviour, so the bug
cannot come back. Several such tests are listed above; they were all written to fail
against the code that had the bug.

## Coverage

Coverage is a floor, not a goal. A test that asserts nothing is worse than no test,
because it inflates the number without adding confidence.

| Package | Coverage | Notes |
|---|---|---|
| `redact` | ~94% | Security-critical; floor is 90% |
| `diagnose` | ~90% | The public code contract |
| `render` | ~93% | Output is where a leak would surface |
| `config` | ~83% | Floor is 80% |
| `cli` | ~77% | Consent gates and exit codes |
| `protocol` | ~70% | Process-heavy; measured behaviourally instead |

CI reports the totals. It does not fail on a percentage, because a threshold that gets
argued about per pull request is worse than one decided once.

## What is deliberately not tested

- **Windows process-group behaviour.** `CREATE_NEW_PROCESS_GROUP` compiles and the
  platform file is exercised on Windows CI, but signal-based group termination has no
  Windows equivalent and is documented as a limitation rather than tested for behaviour
  it cannot have.
- **Unreachable tool-schema states.** The SDK's `AddTool` panics on a tool with no
  `inputSchema`, so that state cannot be produced through the SDK. The validator that
  catches it is tested directly instead, because other SDKs can produce it.
- **Timing-dependent races.** The shutdown ladder is tested for *outcome* (a process
  that ignores stdin close is terminated within the bound) rather than for exact
  timings, which would be flaky.

## Before opening a pull request

```sh
make verify
```

This runs formatting, vet, tests, race, govulncheck, and cross-compilation — the same
gates as CI. CI additionally runs on Windows and macOS, which is where the platform
split in `internal/protocol` is actually exercised.