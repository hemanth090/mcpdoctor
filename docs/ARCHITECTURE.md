# Architecture: MCP Doctor

## 1. Shape of the System

```
                         ┌──────────────────────────┐
   operator flags ──────▶│  internal/cli            │
                         │  parse · validate · exit │
                         └────┬────────┬────────┬───┘
                              │        │        │
             ┌────────────────┘        │        └───────────────┐
             ▼                         ▼                        ▼
   ┌───────────────────┐   ┌─────────────────────┐   ┌──────────────────┐
   │ internal/config   │   │ internal/protocol   │   │ internal/render  │
   │ discovery         │   │ probe (consented)   │   │ human · json     │
   │ client adapters   │   │ stdio · http        │   │ schema v1        │
   │ json + positions  │   │ process lifecycle   │   └────────┬─────────┘
   └─────────┬─────────┘   └──────────┬──────────┘            │
             │                        │                       │
             └───────────┬────────────┴───────────────┬───────┘
                         ▼                            ▼
                ┌─────────────────────┐      ┌──────────────────┐
                │ internal/diagnose   │─────▶│ internal/redact  │
                │ findings · codes    │      │ secret scrubbing │
                │ severity · rules    │      └──────────────────┘
                └──────────┬──────────┘
                           │
                           ▼
                    Report + exit code
```

`config` and `protocol` are independent. Either can run alone. Both emit
`diagnose.Finding` values, which is the only thing the renderers know how to display.

## 2. Module Responsibilities

### `internal/diagnose` - the contract everything else speaks

Leaf module. Defines `Finding`, `Severity`, `Code`, and the aggregation logic that
turns findings into an exit code. It knows nothing about JSON files, processes, or
terminals, which is why it is trivially unit-testable and never needs a real MCP server.

This module is the reason the tool is scriptable: a finding is a stable, documented
value, not a formatted string someone greps for.

### `internal/redact` - leaf module

A registry of secret values and pattern matchers, plus `Redact(string) string`. Every
path that can emit user-influenced text calls it. Being a leaf with no dependencies
means there is no way for a caller to accidentally route around it.

### `internal/config` - discovery and static validation

Two responsibilities kept in one package because they share the file-reading path:

- **Discovery** - enumerate candidate paths per client per OS, plus project-level files.
  Returns `[]Source` describing *what could be read*, without reading.
- **Loading** - read (bounded, read-only, symlink-resolved) and parse JSON **with line
  and column numbers on every error**. Preserving positions is what makes a diagnostic
  actionable, and it is the reason we use a tokenising pass rather than plain
  `encoding/json` for error reporting.

Validation rules live here. Each rule is a small function returning zero or more
findings, registered in one table. Adding a rule means adding a function and one table
row - no dispatch changes.

### `internal/protocol` - the consented live probe

Implements `probe.Probe(ctx, Target, Options) (Report, error)`.

- **Process management** owns the subprocess. It builds an explicit environment
  allowlist, starts the child in its own process group, captures stderr into a bounded
  ring buffer, and implements the spec's shutdown ladder. It does **not** read or write
  the protocol streams itself.
- **Transports** wrap a live connection as an `mcp.Transport`. The stdio transport
  hands the SDK the child's stdin/stdout and owns nothing else, so diagnostic output can
  never corrupt the protocol stream - a hard requirement, because a stray write to a
  server's stdout is a protocol violation that would corrupt the very diagnosis.

Correctness of `initialize`, capability negotiation, and `tools/list` comes from the
official SDK. We do not reimplement JSON-RPC. Our value-add is the *diagnosis* around
it: timeouts classified correctly, stderr surfaced as a finding, oversized messages
bounded, and every failure mapped to a stable code.

### `internal/render` - output, and nothing else

Two renderers implementing one interface. Both are pure functions over a `Report`. The
JSON renderer emits a versioned schema; the human renderer emits text. Neither performs
diagnostics or I/O, so both are testable with a literal expected string.

## 3. Key Decisions

| Decision | Choice | Why | Rejected |
|---|---|---|---|
| Protocol implementation | Official Go SDK v1.8.0 | Version negotiation is subtle (server may answer a different version; newer revisions add `server/discover`). Reimplementing it would be slower and less correct. | Hand-rolled JSON-RPC |
| Executing servers | Own `os/exec`, not SDK's `CommandTransport` | We need process groups, bounded stderr capture, an explicit env allowlist, and a precise shutdown ladder. | `mcp.CommandTransport` (no stderr hook, no process-group control) |
| Interactive consent | Default-on, `--yes` to skip | CI is non-interactive; humans are not. | Always-on prompt (breaks CI) / never (unsafe) |
| Findings | Stable string codes, additive only | Downstream tooling can match `MDC1009` forever. Integer codes get renumbered by accident. | Integers |
| JSON parse errors | Custom position-tracking scan | `encoding/json`'s `SyntaxError.Offset` is a byte offset into the whole file; users need `file:line:col`. | Plain `json.Unmarshal` |
| Secrets | Registry + pattern scrubbing, applied at every sink | Centralising it means a new output path cannot forget. | Redacting at the point of reading (misses server output) |
| Parallelism | Sequential server probes | Simpler determinism, bounded resource use. A config with 50 servers is not a real workload. | Worker pool |
| Global state | None | Test isolation and race-freedom. `go test -race` is a hard gate. | Package-level singletons |

## 4. Control Flow: `probe`

```
1. Validate target (scheme, host, consent state).
2. Build child env from allowlist ∩ config env.        ← secrets registered here
3. Start child in its own process group.
4. Start bounded stderr ring-buffer reader.
5. Wrap stdin/stdout as an mcp.Transport.
6. ctx, cancel := context.WithTimeout(parent, opts.Timeout)  ← no unbounded wait
7. cs, err := client.Connect(ctx, transport)
     err → classify:
        process gone      → MDC2002 (include stderr excerpt)
        deadline exceeded → MDC2003 (include stderr excerpt)
        else              → MDC2004/2005 (include excerpt)
8. negotiated := cs.InitializeResult()
     version unknown to us → MDC2006 finding (informational, not fatal)
9. For each capability we care about, probe it:
     tools advertised → tools/list, with a per-call deadline
     invalid tool schema → MDC2009
10. cs.Close()  → SDK's ladder: close stdin, wait, SIGTERM, SIGKILL
11. Wait for child with a bounded grace period; kill the process group if needed.
12. Reap stderr ring buffer, redact, attach as a finding if non-empty and informative.
```

Every arrow is a place a test exists for. Steps 7-9 are the ones that historically
produce silent hangs, so each is deadline-bounded independently.

## 5. Determinism

Diagnostics should be reproducible: same input, same output, same exit code.

- Servers are probed in sorted order, not map order.
- Findings are sorted by (code, target) before rendering.
- No timestamps inside the finding set. The report carries one `generatedAt` field, and
  it is the only non-deterministic byte in the output.
- stderr excerpts are captured from a ring buffer, so the excerpt for a chatty server
  is the *most recent* N bytes - deterministic given the same byte stream.

## 6. Error Taxonomy

Three layers, kept distinct because they mean different things to a user:

| Layer | Example | Reported as |
|---|---|---|
| Usage | unknown flag | exit 2, plain stderr, no report |
| Operational | config unreadable, network unreachable | finding if attributable to a target, else exit 2 |
| Diagnostic | missing field, protocol mismatch | finding with code + severity |

The distinction is the point: "your config says `cmd` instead of `command`" is a finding
with a fix; "I could not read that file" is an operational error, because there is no fix
to print.

## 7. Extension Points

Deliberately few. Each exists because something already needed it:

- **`config.Adapter`** - one per supported client. Adding a client is a struct and a
  registry entry.
- **`protocol.Transport`** - one per transport. Stdio and HTTP ship in v1.
- **`diagnose.Rule`** - a validation function. Registry-ordered.

There is no plugin system, no dynamic rule loading, and no client adapter loaded from a
config file. Those would be architecture for hypothetical scale, and the product
requirement is explicit about avoiding that.