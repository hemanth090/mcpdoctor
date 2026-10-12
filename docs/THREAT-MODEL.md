# Threat Model: MCP Doctor

## 1. Scope

MCP Doctor reads MCP client configuration files and, **only with explicit user
authorisation**, starts the server commands those files describe and speaks MCP to them
over stdio or Streamable HTTP.

Out of scope: vulnerabilities in MCP servers themselves (we report them, we do not fix
them), the security of the MCP protocol itself, and the user's own environment.

## 2. Assets

| Asset | Why it matters |
|---|---|
| Secrets in config `env` and `headers` | API keys, tokens, passwords. Leaking one is an incident. |
| Host filesystem | A malicious command could read or destroy it. |
| Local network / internal endpoints | A malicious URL could probe or exfiltrate to them. |
| Terminal integrity | Control characters in server output could spoof prompts or hide text. |
| CI trust | A green exit code that lies is worse than a red one. |

## 3. Trust Boundaries

```
  ┌─────────────────────────────────────────────────────────────┐
  │ TRUSTED: our code, our flags, the user's explicit decisions │
  └─────────────────────────────────────────────────────────────┘
        │  B1: read untrusted file
        ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ UNTRUSTED: config file bytes — arbitrary JSON from any of  │
  │ several third-party clients, possibly edited by a repo      │
  └─────────────────────────────────────────────────────────────┘
        │  B2: explicit --execute + consent (the critical gate)
        ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ UNTRUSTED + ARBITRARY CODE: the MCP server subprocess       │
  │  - writes to stdout/stderr                                  │
  │  - may be hostile, buggy, or hijacked                       │
  └─────────────────────────────────────────────────────────────┘
        │  B3: network (Streamable HTTP only)
        ▼
  ┌─────────────────────────────────────────────────────────────┐
  │ UNTRUSTED: remote MCP endpoint responses                     │
  └─────────────────────────────────────────────────────────────┘
```

Three boundaries, each with controls below. **B2 is the one that matters most** — it is
the only path to arbitrary code execution.

## 4. Threats and Controls

### T1 — Config file directs execution of an attacker-chosen command

A malicious or careless repo ships `.mcp.json` naming `curl … | sh`. Claude Desktop and
other clients load project configs automatically.

- **Control:** the tool never executes a command it did not receive from the operator on
  the command line or via an interactive prompt. `check` performs zero execution, full
  stop. `probe` requires `--execute`; in a terminal it prints the exact argv and waits
  for confirmation. `--yes` is the explicit non-interactive override and is documented
  as such.
- **Control:** `check` *reports* a finding describing what a client would execute, so a
  reviewer can see the blast radius before the client ever runs it.
- **Residual risk:** an operator who passes `--execute --yes` in CI authorises whatever
  the config says. This is documented, not mitigated.

### T2 — Command/argument injection

Building a shell string from config would let `args: ["foo; rm -rf /"]` become code.

- **Control:** never use a shell. `os/exec` with an explicit argv slice; on Windows,
  `SysProcAttr{CmdLine}` is left unset so Go applies its own argument escaping.
- **Control:** validate that `command` is a bare executable name or an absolute path, and
  reject paths containing NUL or other control characters.

### T3 — Secret disclosure

Config `env` and `headers` routinely contain API keys. They can leak into terminal
scrollback, CI logs, JSON reports, or a pasted GitHub issue.

- **Control:** every value sourced from config `env`/`headers` is registered with the
  redactor before any output is produced. Human renderer, JSON renderer, and the error
  path all route through it.
- **Control:** server output (stderr, and any captured message text) is scrubbed for
  registered secret values *and* for high-entropy/known-prefix token shapes before it is
  included in a finding.
- **Control:** a value is reported as `***redacted***`; its *length* may be reported, its
  content never.
- **Control:** golden test files contain synthetic secrets (`test-SECRET-value`) only.
  A test asserts these strings never appear in any renderer output.
- **Residual risk:** we cannot detect a secret that is, for example, a password the user
  typed into a tool argument. We redact what we know about.

### T4 — Malicious or malformed server output

The server is arbitrary code. It controls its stdout and stderr completely.

- **Control:** all reads are bounded. `bufio.Scanner` with an explicit maximum line length
  (4 MiB default); HTTP bodies capped via `io.LimitReader`; config files capped by size
  before parsing.
- **Control:** output that is not a valid JSON-RPC message produces a finding with a
  redacted, length-capped excerpt — never a raw dump.
- **Control:** control characters (including ANSI escapes) are stripped from anything we
  echo, so a hostile server cannot repaint the terminal or forge a diagnostic line.
- **Control:** server stderr is captured into a **bounded ring buffer**, so a server that
  logs without limit cannot exhaust memory.

### T5 — Filesystem path abuse

`cwd`, `command`, and config paths are attacker-influenced strings.

- **Control:** all filesystem access goes through a validating accessor that rejects NUL
  bytes, resolves symlinks, and compares the result against the intended root.
- **Control:** symlinked config files are resolved and reported — we tell the user which
  real file was read, because a symlink in a shared directory is itself a finding.
- **Control:** we open config files read-only and never open them for writing, because we
  never write.

### T6 — Network abuse via Streamable HTTP

A hostile `url` could target an internal metadata endpoint or a loopback admin port.

- **Control:** the HTTP client has an explicit connect, TLS-handshake, response-header,
  and overall timeout. No transport keeps a connection open indefinitely.
- **Control:** redirects are followed at most 3 times, and the post-redirect scheme is
  re-validated so `https → http` downgrade is reported as a finding rather than silently
  accepted.
- **Control:** non-HTTP(S) schemes are rejected outright at config-validation time.
- **Control:** we send no ambient credentials, no cookies, and no auth headers beyond
  those the operator's config specifies.
- **Control:** `--allow-remote` is required before probing a host that is not loopback,
  because "my config points at a public URL" is usually a mistake worth confirming.
- **Residual risk:** with operator consent we will connect to whatever URL they named.

### T7 — Denial of service / resource exhaustion

- **Control:** every blocking operation takes a `context.Context` with a deadline.
  Ctrl-C cancels promptly; nothing hangs forever.
- **Control:** the number of servers probed per run is capped, and probing is sequential
  — never a fan-out of unbounded subprocesses.
- **Control:** shutdown follows the spec's ladder: close stdin → wait → `SIGTERM` →
  wait → `SIGKILL`. A server that ignores `SIGTERM` is still reaped.
- **Control:** the subprocess is started in its own process group (`Setpgid` /
  `CREATE_NEW_PROCESS_GROUP`) so we can signal the whole tree, and so a server that
  daemonises does not outlive us unnoticed.

### T8 — CI trust erosion

- **Control:** exit codes are a documented, tested contract. `1` means findings at or
  above the threshold, and never means "crashed".
- **Control:** an internal error exits `2`, so a pipeline can distinguish "your config has
  problems" from "the doctor broke".
- **Control:** `--fail-on <severity>` lets a team pick its own bar; the default is
  `error`.
- **Control:** no telemetry, no remote logging, no crash upload. The tool makes network
  requests only to a probed endpoint the operator named.

### T9 — TOCTOU on config between check and probe

- **Control:** the parsed config is the single source of truth for the run. We re-read
  only if the user asks for a second pass, and we report the file's mtime/size in the
  report so drift is visible.

### T10 — Unbounded or absurd resource use by the operator's own config

- **Control:** limits are constants in `internal/config/limits.go`, documented, and
  overridable downward (never upward) by flags.

## 5. Explicit Non-Goals — Honest Limitations

**This tool does not sandbox.** `os/exec` starts a normal child process with the
invoking user's full privileges. A hostile MCP server can read every file the user can
read, make every network call the user can make, and write wherever the user can write.
Process-group isolation lets us *clean up* a child's descendants; it does not *contain*
them.

The honest statement of what we do provide:

| Property | Provided? | How |
|---|---|---|
| Consent before execution | Yes | `--execute` + interactive prompt |
| No shell interpretation | Yes | argv-based `os/exec` |
| Credential isolation | Yes | we build the child env from an explicit allowlist, not `os.Environ()` |
| Bounded memory and time | Yes | read caps, ring buffer, deadlines |
| Filesystem confinement | **No** | child inherits user privileges |
| Network confinement | **No** | child inherits user network access |
| Privilege dropping | **No** | we do not drop privileges |

Users who need containment should run MCP Doctor itself inside a container or VM. We say
so in the README rather than implying a safety guarantee we do not provide.

## 6. Security Test Coverage

Each control above has a corresponding test. See `docs/TESTING.md` for the mapping table
and the command that runs them.

## 7. Reporting a Vulnerability

See `SECURITY.md`. Do not open a public issue for a security report.