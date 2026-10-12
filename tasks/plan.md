# Implementation Plan: MCP Doctor v1.0.0

## Overview
A single-binary Go CLI that statically validates MCP client configurations and,
under explicit consent, live-probes MCP servers over stdio and Streamable HTTP.

## Architecture Decisions
See docs/ARCHITECTURE.md. Summary: official Go SDK for protocol correctness, own
process management for control, leaf `diagnose` + `redact` modules, zero global state.

## Task List

### Phase 0: Foundation
- [ ] T1 Module, quality gates, CI skeleton
- [ ] T2 `diagnose` findings model, codes, severity, exit codes
- [ ] T3 `redact` registry and scrubbers

### Phase 1: Configuration
- [ ] T4 Position-tracking JSON reader
- [ ] T5 Client adapters + discovery
- [ ] T6 Static validation rules

### Phase 2: Protocol
- [ ] T7 Bounded line reader + process lifecycle
- [ ] T8 stdio transport + probe flow
- [ ] T9 Streamable HTTP transport + endpoint validation

### Phase 3: Output
- [ ] T10 Renderers (human + versioned JSON)
- [ ] T11 CLI commands, consent flow, exit codes

### Phase 4: Assurance
- [ ] T12 Fuzz targets
- [ ] T13 Security tests
- [ ] T14 Mock MCP server test harness
- [ ] T15 Cross-platform verification

### Phase 5: Release
- [ ] T16 CI workflows, release pipeline
- [ ] T17 Documentation set

## Risks and Mitigations
| Risk | Impact | Mitigation |
|---|---|---|
| Client config formats undocumented/changing | False positives | Warn on unknown keys; document per-client support explicitly |
| Untrusted server output corrupting stdout protocol | Wrong diagnosis | stderr captured separately, bounded; never write diagnostics to child stdout |
| Process hang | CI stuck | Per-request deadlines + spec shutdown ladder + process group kill |
| Secret leakage into reports | Incident | Central redaction at every sink; dedicated leak tests |
| Go SDK API churn | Build breakage | Pin exact version; CI gate on build |

## Open Questions
None blocking. Compatibility decisions recorded in docs/COMPATIBILITY.md.
