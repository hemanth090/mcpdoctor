# MCP Doctor user guide

Installation, every command, and worked examples of diagnosing real failures.

## Contents

- [Install](#install)
- [Commands](#commands)
- [Worked examples](#worked-examples)
- [Reading the output](#reading-the-output)
- [Using it in CI](#using-it-in-ci)
- [Permissions and consent](#permissions-and-consent)
- [Troubleshooting](#troubleshooting)

## Install

```sh
go install github.com/mcpdoctor/mcpdoctor/cmd/mcpdoctor@latest
```

Or download a binary from the
[releases page](https://github.com/mcpdoctor/mcpdoctor/releases). Each release includes
per-platform binaries and a `checksums.txt`.

Verify a downloaded binary:

```sh
sha256sum -c checksums.txt --ignore-missing
mcpdoctor --version
```

Prebuilt binaries are static (`CGO_ENABLED=0`) and run without Go installed. Building
from source needs Go 1.25 or newer.

## Commands

### `mcpdoctor check`

Validates configuration. **Starts nothing, contacts nothing.**

```sh
mcpdoctor check                              # every config it can find
mcpdoctor check --config .mcp.json           # one specific file
mcpdoctor check --project ~/src/my-repo      # include project configs
mcpdoctor check --json                       # machine-readable
mcpdoctor check --fail-on warning            # stricter gate
```

| Flag | Meaning |
|---|---|
| `--config PATH` | Examine exactly this file. Repeatable. Implies nothing else is checked |
| `--project DIR` | Also scan `DIR` for project-level configs. Defaults to the working directory |
| `--json` | Emit the [JSON report](SCHEMA.md) |
| `--fail-on LEVEL` | Lowest severity that fails the run: `error` (default), `warning`, `info` |
| `--quiet` | Suppress progress notes; findings are always printed |

### `mcpdoctor probe`

Starts servers and checks the MCP conversation.

```sh
mcpdoctor probe my-server --execute           # interactive, confirms first
mcpdoctor probe --execute --yes               # non-interactive
mcpdoctor probe my-server --execute --timeout 5s
```

| Flag | Meaning |
|---|---|
| `--execute` | Authorise starting the servers. Without it, nothing runs |
| `--yes` | Skip the confirmation. Required with `--execute` when stdin is not a terminal |
| `--allow-remote` | Authorise contacting a server that is not on this machine |
| `--timeout D` | Bound on a single probe. Default `20s` |

### `mcpdoctor clients`

Lists every configuration location MCP Doctor knows about on this platform, and whether
a file was found there. Reads only the filesystem.

```sh
mcpdoctor clients
mcpdoctor clients --project ~/src/my-repo
mcpdoctor clients --json
```

### `mcpdoctor docs`

Prints the exit-code contract. The same content ships in the binary so it is available
without the repository.

## Worked examples

### "Connection closed" in my client

This is the most common report, and it is almost never a network problem.

```sh
mcpdoctor check
```

Look for `MDC1006` (command not found) or `MDC1002` (no transport). Then:

```sh
mcpdoctor probe my-server --execute
```

`MDC2002` means the server exited during startup, and the finding includes the tail of
its stderr, which is where the real reason is.

### The server prints a banner and everything breaks

```sh
mcpdoctor probe my-server --execute
```

The specification reserves stdout for protocol messages. A server that writes a startup
banner there breaks every client that parses stdout strictly. The finding names stdout
explicitly because the fix is on the server side, not in your configuration.

### My committed config fails in CI but works locally

```sh
mcpdoctor check --config .mcp.json --fail-on warning
```

Usually `MDC1006`: the command resolves against `PATH`, which differs between your
machine and the runner. An absolute path fixes it.

### I think I leaked a key

```sh
mcpdoctor check --config .mcp.json --json | jq '.findings[] | select(.code == "MDC1020")'
```

MCP Doctor reports that a value *looks* like a credential. It never prints the value. If
the file is committed, rotate the credential - git history retains it after a later
commit removes it.

### A client supports a config this tool does not read

Continue uses YAML and OpenAI Codex uses TOML. MCP Doctor reports `MDC0011` for these:
detected, not parsed. This is stated rather than hidden because a tool that silently
ignores half your configuration is worse than one that tells you.

## Reading the output

Each finding carries:

```
[MDC1006] command "server" was not found on PATH
    target: my-server
      The client resolves a bare command name against the PATH it inherits.
    fix: Install the executable so it is on PATH, or use its absolute path.
    example: "command": "/usr/local/bin/server"
```

- **`MDC1006`** - a stable code. Safe to match on in automation, forever.
- **`target`** - which server or file.
- **`fix`** - one action. If there is more to say, it is in `detail`.
- **`example`** - copyable. Already redacted.

Findings are grouped by severity, errors first.

### Codes

| Range | Concern |
|---|---|
| `MDC0xxx` | Finding and reading the configuration file |
| `MDC1xxx` | Configuration problems |
| `MDC2xxx` | Live protocol problems |

Run `mcpdoctor check --json` to see the codes that actually apply to you. The full list
with meanings is in `internal/diagnose/codes.go`.

## Using it in CI

```sh
# Block a merge on a broken configuration
mcpdoctor check --config .mcp.json
```

Exit codes:

| Code | Meaning |
|---|---|
| `0` | Clean |
| `1` | Your configuration has problems |
| `2` | MCP Doctor could not run - bad flag, unreadable input |
| `130` | Cancelled |

Treat `2` as a distinct failure: it means the check did not happen, which is not the same
as the check passing.

```sh
# Fail with detail, and let a human read it
if ! mcpdoctor check --config .mcp.json --json > report.json; then
  case $? in
    1) echo "::error::MCP configuration has problems"
       jq -r '.findings[] | select(.severity == "error") |
              "  \(.location // .target): \(.message) [\(.code)]"' report.json ;;
    2) echo "::error::mcp-doctor could not run" ;;
  esac
  exit 1
fi
```

`check` is safe against untrusted repositories: it contains no process-spawning code
path at all. A test asserts that a config naming a marker-file-creating command does not
cause that file to exist after a run.

Probing in CI is different: it runs whatever the config says.

```sh
mcpdoctor probe --execute --yes   # only if you trust the repository
```

## Permissions and consent

`check` needs read access to the config files it checks. Nothing more.

`probe` needs to start processes. Before the first one, MCP Doctor prints the exact
command and waits:

```
The following 1 server(s) will be started. Each runs code from your configuration:

  filesystem: npx -y @modelcontextprotocol/server-filesystem /tmp

Start them? [y/N]
```

Pressing Enter declines. There is no default-yes.

Without `--yes` in a non-interactive context, the tool refuses:

```
mcp-doctor: refusing to start 1 server(s) without confirmation.
This is a non-interactive context, so there is nobody to ask.
Pass --execute --yes if you intend to run: filesystem
```

This is deliberate: CI should record the authorisation visibly rather than assume it.

Secrets in arguments are redacted in the prompt.

A remote endpoint is a separate decision. Even after `--execute`, contacting a host that
is not on your machine needs `--allow-remote` or its own confirmation.

## Troubleshooting

### `check` finds nothing

```sh
mcpdoctor clients
```

Lists the paths that were checked. If your client's file is not there, MCP Doctor does
not know about that client. See
[COMPATIBILITY.md](COMPATIBILITY.md) and open an issue if it should be supported.

### Everything is reported as "not found"

Usually correct: you simply have no configuration for most clients. This is
informational (`MDC0001`) and does not fail a run.

### `MDC1018` unknown field

A key MCP Doctor does not recognise. Almost always a client that added a field. It is a
warning, not an error. If MCP Doctor does not know a real field, that is a gap worth
reporting.

### `MDC1017` transport type not recognised

Your client has documented type values that differ from the ones you used. Cline spells
it `streamableHttp`; Roo Code uses `streamable-http`; Cursor requires `type: "stdio"`.
The finding suggests the value your client documents.

### The probe says the server timed out

`MDC2003`. Start it by hand:

```sh
cd <the working directory from your config>
<the command from your config>
```

It usually blocks on a network call or a missing credential. `--timeout` can be raised,
but a server that needs more than 20 seconds to say hello usually has a real problem.

### Everything is redacted and I cannot see the value

Correct behaviour. MCP Doctor tells you *which* variable holds a credential and never
the credential itself. If you need the value, it is in your configuration file.

## Related

- [Compatibility matrix](COMPATIBILITY.md) - supported clients, paths, and protocol versions
- [Security model](THREAT-MODEL.md) - what is and is not protected
- [JSON schema](SCHEMA.md) - the report format