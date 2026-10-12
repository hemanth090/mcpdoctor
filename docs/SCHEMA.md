# JSON report schema

`mcpdoctor --json` emits a single JSON document.

## Compatibility guarantee

Within a **major version** of the schema:

- Fields are never removed.
- Field meanings never change.
- New fields may be added, and will be optional or have a documented default.

Consumers should **ignore unknown fields**, which is what lets new fields ship without
breaking anyone.

A breaking change increments the major version. `schemaVersion` is a string, not a
number, so the versioning scheme itself can evolve.

## Document

```json
{
  "schemaVersion": "1.0",
  "tool": "mcp-doctor",
  "toolVersion": "1.0.0",
  "generatedAt": "2026-10-12T09:30:00Z",
  "summary": { "errors": 2, "warnings": 3, "infos": 1 },
  "sources": [],
  "servers": [],
  "findings": []
}
```

| Field | Type | Meaning |
|---|---|---|
| `schemaVersion` | string | Shape of this document |
| `tool` | string | Always `mcp-doctor` |
| `toolVersion` | string | Version that produced the document |
| `generatedAt` | string | RFC 3339 UTC timestamp |
| `summary` | object | Finding counts by severity |
| `sources` | array | Every configuration location examined |
| `servers` | array | Probed server results |
| `findings` | array | Diagnostics, sorted |

### Determinism

`generatedAt` is the **only** non-deterministic value in the document. Everything else
is byte-for-byte reproducible for the same input, so two runs can be diffed directly in
CI. Findings are sorted by severity, then code, then target.

## `summary`

```json
{ "errors": 2, "warnings": 3, "infos": 1 }
```

## `sources[]`

Every configuration location examined, whether or not the file existed. This is what
distinguishes "everything passed" from "nothing was found".

```json
{
  "client": "claude-code",
  "path": "/repo/.mcp.json",
  "scope": "project",
  "format": "json",
  "exists": true,
  "read": true,
  "symlinkTarget": "/shared/config.json",
  "error": "",
  "serverCount": 3
}
```

| Field | Type | Notes |
|---|---|---|
| `client` | string | Client id, or `explicit` for `--config` |
| `path` | string | Redacted absolute path |
| `scope` | string | `user` or `project` |
| `format` | string | `json`, `yaml`, or `toml` |
| `exists` | bool | Whether the file was present |
| `read` | bool | Whether it parsed successfully |
| `symlinkTarget` | string | Present only when the path is a symlink |
| `error` | string | Present only on a read or parse failure |
| `serverCount` | number | Servers found |

## `servers[]`

```json
{
  "name": "filesystem",
  "transport": "stdio",
  "probed": true,
  "negotiatedVersion": "2026-07-28",
  "serverName": "filesystem-server",
  "serverVersion": "1.2.0",
  "capabilities": ["resources", "tools"],
  "toolCount": 4,
  "durationMs": 1723
}
```

| Field | Type | Notes |
|---|---|---|
| `name` | string | Configured server name |
| `transport` | string | `stdio` or `http` |
| `probed` | bool | `false` when execution was not authorised |
| `negotiatedVersion` | string | Absent when the handshake did not complete |
| `serverName` | string | From the server's `serverInfo` |
| `serverVersion` | string | From the server's `serverInfo` |
| `capabilities` | array | Sorted |
| `toolCount` | number | Tools advertised |
| `durationMs` | number | Wall-clock probe time |

## `findings[]`

```json
{
  "code": "MDC1006",
  "severity": "error",
  "target": "filesystem",
  "location": "/repo/.mcp.json:7:5",
  "message": "command \"server\" was not found on PATH",
  "detail": "The client resolves a bare command name against the PATH it inherits.",
  "remedy": "Install the executable so it is on PATH, or use its absolute path.",
  "example": "\"command\": \"/usr/local/bin/server\""
}
```

| Field | Type | Notes |
|---|---|---|
| `code` | string | Stable identifier; safe to match on |
| `severity` | string | `error`, `warning`, or `info` |
| `target` | string | Server name or config path |
| `location` | string | `path:line:col`, absent when unknown |
| `message` | string | One-line statement of the problem |
| `detail` | string | Why it matters |
| `remedy` | string | The recommended action |
| `example` | string | Copyable snippet, always redacted |

### Severities

| Value | Meaning | Default effect |
|---|---|---|
| `error` | Broken or unsafe | Fails the run |
| `warning` | Probably wrong | Fails with `--fail-on warning` |
| `info` | Context only | Never fails the run |

## Diagnostic codes

Codes are strings and are **never reused or renumbered**. A code that means one thing in
`1.0` means the same thing in `2.0`. If you automate against `MDC1006`, it will keep
working.

| Range | Concern |
|---|---|
| `MDC0xxx` | Configuration discovery and file access |
| `MDC1xxx` | Static configuration validation |
| `MDC2xxx` | Live protocol diagnostics |

The authoritative list is `internal/diagnose/codes.go`. `mcpdoctor check --json` emits
only codes that actually occurred; to enumerate the full set, read that file.

## Redaction

Every string in the document passes through the redactor before it is written. This
includes paths, error messages, and every field of every finding. Credentials from `env`,
`headers`, and `args`, plus anything a server echoes on stderr, appear as
`***REDACTED***`.

The redactor catches two classes of secret: values the tool was *told* about, and values
matching a credential *shape* (provider key prefixes, JWTs, PEM private key blocks,
credentials embedded in a URL). The second class exists because a server may echo a
credential the tool never saw.

## Example: gate a CI job

```sh
mcpdoctor check --config .mcp.json --json > report.json
jq -r '.findings[] | select(.severity == "error") |
       "\(.location // .target): \(.message) [\(.code)]"' report.json
```

```sh
# Fail only on findings a human should look at
mcpdoctor check --config .mcp.json --fail-on error
test $? -eq 0 || { echo "MCP configuration has errors"; exit 1; }
```