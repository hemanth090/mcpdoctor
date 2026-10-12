# MCP Doctor

Diagnose Model Context Protocol (MCP) server configurations, and - with your explicit
permission - actually start the servers and check that they speak MCP.

MCP servers fail in boring, specific ways. The binary isn't installed. The config key is
`cmd` instead of `command`. The server prints a banner to stdout and corrupts its own
protocol stream. MCP Doctor names the problem, points at the line, and tells you the fix.

**It never edits your configuration, and it never runs anything without asking.**

---

## Why

Three things go wrong when an MCP server misbehaves:

1. **The error is somewhere else.** The failure surfaces in the client, but the cause is
   in a config file the client never showed you.
2. **The fix is guesswork.** "Connection closed" does not say which of a dozen things
   went wrong.
3. **You cannot tell a config problem from a tool problem.** This is what exit codes are
   for, and why MCP Doctor uses three of them.

## Install

```sh
go install github.com/mcpdoctor/mcpdoctor/cmd/mcpdoctor@latest
```

Or download a binary for your platform from the
[releases page](https://github.com/mcpdoctor/mcpdoctor/releases), verify it against the
published `checksums.txt`, and put it on your `PATH`.

Requires Go 1.25 or newer to build from source. Prebuilt binaries need no Go.

## Use

### Check a configuration

```sh
mcpdoctor check
```

Discovers every MCP client configuration it knows about on your machine and validates
it. This **never starts a server and never touches the network**, so it is safe to run
against a repository you did not write.

### Check one specific file

```sh
mcpdoctor check --config .mcp.json
```

This is the form to use in CI. It examines exactly the file you name and nothing else.

### See what will be checked

```sh
mcpdoctor clients
```

### Actually start a server and talk to it

```sh
mcpdoctor probe my-server --execute
```

MCP Doctor will print the exact command it intends to run and wait for you to confirm.
This runs someone else's code on your machine, so it is opt-in every time.

In a script or CI, `--execute` must be paired with `--yes`, so the authorisation is
visible in the script rather than implied:

```sh
mcpdoctor probe --execute --yes
```

### Machine-readable output

```sh
mcpdoctor check --json | jq '.findings[] | select(.severity == "error")'
```

## What it checks

### Configuration (no execution)

- Malformed JSON, reported with an exact `file:line:column`
- Duplicate keys in one object - a standard parser silently keeps the last one
- Missing or contradictory transports (`command` and `url` both present, or neither)
- Commands that will not resolve on `PATH`, or that are not executable
- Arguments pasted as a shell line instead of an argv array
- Invalid environment variable names, which clients drop silently
- Missing working directories
- URLs with an unsupported scheme, plain HTTP to a non-local host, or embedded
  credentials
- Transport `type` values that do not match what your client documents
- Values that look like credentials committed to a file
- Configuration files that are symlinks, too large, or unreadable

### Protocol (with `--execute`)

- Server startup and the `initialize` handshake
- Protocol version negotiation
- Capability advertisement versus what the server can actually do
- `tools/list`, with each tool's `inputSchema` validated against the specification
- Duplicate or unnamed tools, which make tool calls ambiguous
- Startup timeouts, crashes, malformed messages, and transport failures, each reported
  as a distinct problem with a distinct fix
- Captured stderr, so the server's own explanation reaches you

### Supported clients

Configuration paths and formats are modelled per client, because they genuinely differ.
See [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) for the full matrix, including which
paths are documented by the vendor and which are inferred.

Claude Desktop · Claude Code · VS Code · Cursor · Windsurf · Zed · Cline · Roo Code ·
Amazon Q Developer CLI · Gemini CLI · GitHub Copilot CLI

**Not parsed in v1:** Continue (YAML) and OpenAI Codex (TOML). Both are *detected* and
reported as unsupported rather than silently ignored.

### Supported transports

| Transport | Status |
|---|---|
| stdio | Static validation and live probing |
| Streamable HTTP | Static validation and live probing |
| Legacy HTTP+SSE | Static validation; the type is recognised, live probing is not supported |

### Protocol versions

`2026-07-28` (current), `2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05`.
Negotiation is handled by the official Go SDK; a server that negotiates something newer
than this build understands is reported as informational, not as an error.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Completed, nothing reached the `--fail-on` threshold |
| `1` | Completed, and something did - your configuration has problems |
| `2` | MCP Doctor could not run: bad flag, unreadable input, internal error |
| `130` | Cancelled with Ctrl-C |

`1` and `2` are deliberately distinct so a pipeline can tell a broken config from a
broken tool. Run `mcpdoctor docs` for the same table with examples.

```sh
# Block a merge on a broken configuration
mcpdoctor check --config .mcp.json

# Be stricter
mcpdoctor check --config .mcp.json --fail-on warning
```

## Security

The full model is in [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md). The short version:

**What MCP Doctor does provide**

- Consent before execution, every time - `--execute` plus an interactive prompt
- No shell, ever: arguments are passed as an argv array, so `;` and `|` are literal
- Credential isolation: the child process gets an allowlisted environment, not your
  whole shell
- Redaction on every output path, so tokens from `env`, `headers`, `args`, and server
  output never reach a report or a log
- Bounded reads, timeouts, and cancellation on every operation
- No telemetry, no remote logging, no crash reporting

**What MCP Doctor does not provide**

- **Sandboxing.** A server started by MCP Doctor runs with your full privileges. It can
  read your files and make your network calls.
- **Filesystem or network confinement** for the child process
- **Privilege dropping**

If you need containment, run MCP Doctor itself inside a container or VM. We would rather
say this plainly than imply a guarantee we cannot keep.

One consequence worth understanding: cloning a repository containing `.mcp.json` puts
someone else's configuration on your machine, and a client may start it automatically.
MCP Doctor's `check` tells you what *would* run, without running it.

## Documentation

- [Installation and usage](docs/README.md)
- [Compatibility matrix](docs/COMPATIBILITY.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Threat model](docs/THREAT-MODEL.md)
- [JSON report schema](docs/SCHEMA.md)
- [Testing strategy](docs/TESTING.md)
- [Development](docs/DEVELOPMENT.md)
- [Security policy](SECURITY.md)

## Contributing

See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md). Run `make verify` before opening a pull
request; it runs the same gates as CI.

## Licence

MIT. See [LICENSE](LICENSE).