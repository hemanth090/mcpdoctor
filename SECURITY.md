# Security Policy

## Reporting a vulnerability

**Do not open a public issue for a security report.**

Email **security@mcp-doctor.dev** with:

- What the issue is, and what an attacker gains
- Steps to reproduce, ideally minimal
- The MCP Doctor version (`mcpdoctor --version`) and your platform
- Whether you have already shared it anywhere publicly

We will acknowledge within 72 hours, assess within 7 days, and keep you updated until it
is fixed or declined. If a report is not a vulnerability - a bug, a bad diagnostic, a
missing platform - we will say so plainly and point you at where to file it.

## Supported versions

Security fixes ship for the current minor release and the previous one.

| Version | Supported |
|---|---|
| `1.x` | Yes |
| `<1.0` | No |

## What MCP Doctor does and does not do

This is the part that matters most, because the risk in this tool is unusual: it runs
code you did not write.

### MCP Doctor will not

- Run a configured command without `--execute` and an explicit confirmation
- Interpret a command through a shell
- Modify, reformat, or "repair" your configuration
- Send telemetry, crash reports, or any data to any server
- Make a network request other than to an MCP endpoint you named and authorised

### MCP Doctor cannot protect you from

**A server you authorise it to run.** An MCP server is an ordinary program. Once started,
it has your user's full privileges: it can read every file you can read, write wherever
you can write, and make any network call you can make.

`os/exec` is not a sandbox. Running MCP Doctor in a container or VM is the only way to
get containment. See [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) for the full analysis,
including the controls that *are* provided and a table of those that are not.

### What is provided instead

- Consent before execution, every time, with the exact command shown
- An allowlisted child environment, so a server does not inherit your credentials
- Redaction of `env`, `headers`, `args`, and server output on every output path
- Bounded reads, timeouts, and cancellation throughout
- Bounded redirects and scheme re-validation on HTTP

## Hardening guidance for CI

If you run MCP Doctor in CI against configurations you do not control:

```sh
# Safe: static analysis only, no execution, no network
mcp-doctor check --config .mcp.json --fail-on error
```

```sh
# Runs code. Only do this if you trust the repository.
mcp-doctor probe --execute --yes
```

`check` is safe by construction, not by policy: it contains no process-spawning or
networking code path at all. A test in `internal/cli` asserts that a configuration
naming a marker-file-creating command does not cause that file to exist after a run.

For defence in depth, consider running MCP Doctor itself in a sandboxed CI step even for
`check`, since it parses JSON from untrusted input.

## Disclosure timeline

- Acknowledgement: 72 hours
- Assessment: 7 days
- Fix target for confirmed high-severity issues: 30 days
- Fix target for moderate issues: next release

We credit reporters in the release notes unless you prefer otherwise.