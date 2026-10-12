# Compatibility

This document records what MCP Doctor actually reads and speaks, and — just as
importantly — where that knowledge came from. Client configuration formats are not
specified centrally; they are each vendor's own choice, and they change without notice.

**Nothing in this document is asserted without a source.** Where a path is inferred
rather than published by the vendor, it says so.

## Protocol revisions

| Revision | Supported | Notes |
|---|---|---|
| `2026-07-28` | Yes | Current revision |
| `2025-11-25` | Yes | Adds Tasks utilities; splits the transport spec pages |
| `2025-06-18` | Yes | First widely deployed revision |
| `2025-03-26` | Yes | |
| `2024-11-05` | Yes | Original; source of the legacy HTTP+SSE transport |

Negotiation is performed by the official Go SDK, which knows all five. MCP Doctor does
not implement version negotiation itself.

If a server negotiates a revision newer than this build recognises, you get an
informational finding (`MDC2006`), not an error: the session may well work fine, and
failing a build for a tool that is merely behind would be wrong.

### Deprecated but still negotiated

Roots and Sampling are deprecated as of `2026-07-28` (SEP-2577) and are scheduled for
removal. The SDKs support them for at least twelve months. MCP Doctor neither requires
nor recommends them.

## Transports

| Transport | Static validation | Live probing | Notes |
|---|---|---|---|
| stdio | Yes | Yes | The default for most servers |
| Streamable HTTP | Yes | Yes | Current HTTP transport |
| HTTP+SSE (`2024-11-05`) | Yes | No | Deprecated upstream; the type is recognised and reported |
| WebSocket | Recognised as a `type` value | No | Documented by Claude Code; not an MCP standard transport |
| Unix socket / named pipe | No | No | Documented by VS Code; not implemented in v1 |

### Supported HTTP endpoints

`https` anywhere, and `http` on loopback (`localhost`, `127.0.0.0/8`, `::1`). Plain
`http` to a non-loopback host is reported as an error (`MDC1015`) because it exposes
credentials and tool traffic in cleartext.

## Client configuration matrix

Paths are per platform. A path is only probed on the platform it belongs to.

### Confidence

| Marking | Meaning |
|---|---|
| **Documented** | Published by the vendor or in the official MCP documentation |
| **Vendor docs, inferred path** | The client and format are documented, but the absolute path is composed from two sources |
| **Inferred** | Not published anywhere authoritative; included because it is the path the software actually uses |

### Claude Desktop — **Documented**

| Platform | Path |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |
| Linux | `$XDG_CONFIG_HOME/Claude/claude_desktop_config.json` or `~/.config/Claude/claude_desktop_config.json` |

Root key `mcpServers`. Fields: `command`, `args`, `env`. No `type` field; the transport
is inferred. No project-level configuration.

*Source: official MCP "connect to local servers" documentation for macOS and Windows;
the Linux path is published in VS Code's MCP configuration reference.*

### Claude Code — **Documented**

| Scope | Path |
|---|---|
| Project | `.mcp.json` |
| User | `~/.claude.json`, under `projects["<abs path>"].mcpServers` |
| Local | `~/.claude.json`, top-level `mcpServers` |

Root key `mcpServers`. Accepts `type` values `stdio`, `http`, `sse`, `ws`, and
`streamable-http` as an alias for `http`. Also `headers`, `headersHelper`, `timeout`,
`alwaysLoad`.

An entry with a `url` and no `type` is an **error** in Claude Code (it is read as stdio).
MCP Doctor reports this as `MDC1017` rather than treating it as a stylistic issue.

Interpolation (`${VAR}`, `${VAR:-default}`) is documented for `command`, `args`, `env`,
`url`, and `headers`. Support differs between clients, so MCP Doctor reports an
interpolation placeholder as a warning (`MDC1021`) rather than assuming it works.

*Source: `docs.claude.com/en/docs/claude-code/mcp`.*

### VS Code / GitHub Copilot — **Documented** (user-profile path inferred)

| Location | Root key |
|---|---|
| `.vscode/mcp.json` | `servers` |
| `.mcp.json` (workspace root) | `mcpServers` |
| `mcp.json` in the user profile folder | `servers` |
| `$COPILOT_HOME/mcp-config.json` or `~/.copilot/mcp-config.json` | `mcpServers` |

Fields: `type` (required: `stdio`, `http`, `sse`), `command`, `args`, `cwd`, `env`,
`envFile`, `headers`, `oauth`, `sandboxEnabled`, `dev`.

No `disabled` field exists in the file; enablement lives in settings.

The user-profile path is composed from the documented settings directory plus the
documented `mcp.json` filename. VS Code's MCP documentation says only "the `mcp.json`
file in your user profile folder".

*Sources: `code.visualstudio.com/docs/agent-customization/mcp-servers`,
`code.visualstudio.com/docs/agents/reference/mcp-configuration`,
`code.visualstudio.com/docs/configure/settings`.*

### Cursor — **Documented**

| Scope | Path |
|---|---|
| Global | `~/.cursor/mcp.json` |
| Project | `.cursor/mcp.json` |

Root key `mcpServers`. `type: "stdio"` is **required** for local servers. Remote servers
use `url` plus `headers` or `auth`. Interpolation uses `${env:NAME}` and
`${workspaceFolder}`.

*Source: `cursor.com/docs/mcp`.*

### Windsurf — **Documented**

| Purpose | Path |
|---|---|
| Editor discovery | `~/.codeium/windsurf/mcp_config.json` |
| Managed config | `~/.config/devin/mcp_config.json` (macOS/Linux), `%AppData%/devin/mcp_config.json` (Windows) |

Root key `mcpServers`. No `type` field. The URL key is **`serverUrl`**, not `url` — a
difference that silently breaks naive tooling. Extra field: `disabledTools`.

*Source: `docs.devin.ai/windsurf/plugins/cascade/mcp` (Windsurf is now under
Cognition/Devin; the previous docs URL redirects here).*

### Zed — **Documented**

| Platform | Path |
|---|---|
| macOS / Linux | `~/.config/zed/settings.json` |
| Windows | `%APPDATA%\Zed\settings.json` |
| Project | `.zed/settings.json` |

Root key **`context_servers`**, inside the general Zed settings file rather than a
dedicated MCP file. No `type` field. Enablement is a UI toggle.

Zed supports Tools and Prompts only.

*Source: `zed.dev/docs/ai/mcp.md`.*

### Cline — **Documented**

Global: `~/.cline/data/settings/cline_mcp_settings.json` (override with
`CLINE_MCP_SETTINGS_PATH`).

Root key `mcpServers`. Transport values use **camelCase**: `streamableHttp`, `sse`,
`stdio`. Omitting `type` on a remote server defaults to legacy SSE, which is a
documented footgun.

**No project-level MCP configuration file is documented.** `.cline/` exists, but no MCP
file is listed in it.

*Source: `docs.cline.bot/mcp/mcp-overview`.*

### Roo Code — **Vendor docs, inferred path**

| Scope | Path |
|---|---|
| Project | `.roo/mcp.json` |
| Global | `<VS Code user dir>/globalStorage/rooveterinaryinc.roo-cline/settings/mcp_settings.json` |

Root key `mcpServers`. Transport values use **kebab-case**: `streamable-http`, `sse`,
`stdio`. Omitting `type` on a remote server is an immediate error.

The global path is assembled from the VS Code `globalStorage` convention and the
extension's publisher identifier. The publisher slug has changed across releases, which
makes this the least stable path in this table.

*Source: `docs.roocode.com/features/mcp/using-mcp-in-roo`.*

### Amazon Q Developer CLI — **Documented**

| Purpose | Path |
|---|---|
| Legacy global | `~/.aws/amazonq/mcp.json` |
| Legacy project | `.amazonq/mcp.json` |
| IDE GUI | `~/.aws/amazonq/default.json` and `.amazonq/default.json` |

Root key `mcpServers`. `type: "http"` for remote servers. The legacy `mcp.json` files are
gated by `useLegacyMcpJson`, which defaults to **true**.

The precedence between `cli-agents/`, `default.json`, and `mcp.json` is not spelled out
in the documentation.

*Source: `docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/command-line-mcp-config-CLI.html`.*

### Gemini CLI — **Documented**

| Scope | Path |
|---|---|
| User | `~/.gemini/settings.json` |
| Project | `.gemini/settings.json` |

Root key `mcpServers`. **No `type` field.** The transport is selected by which key is
present, and the three URL spellings mean different things:

| Key | Transport |
|---|---|
| `command` | stdio |
| `httpUrl` | Streamable HTTP |
| `url` | Legacy SSE |

Also `trust` (bypasses confirmations), `includeTools`, `excludeTools`, `timeout` (ms),
`cwd`, `headers`.

*Source: `google-gemini.github.io/gemini-cli/docs/tools/mcp-server.html`.*

### GitHub Copilot CLI — **Documented**

`$COPILOT_HOME/mcp-config.json` or `~/.copilot/mcp-config.json`. Portable format, root
key `mcpServers`.

*Source: VS Code MCP configuration reference.*

### Continue — **Documented, not parsed**

| Scope | Path |
|---|---|
| User | `~/.continue/config.yaml` |
| Project | `.continue/mcpServers/` |

**YAML, not JSON.** `mcpServers` is a *list*, not an object. Transport values: `sse`,
`stdio`, `streamable-http`.

MCP Doctor detects this configuration and reports it as unsupported (`MDC0011`). It does
not parse it.

*Source: `docs.continue.dev/customize/deep-dives/mcp`.*

### OpenAI Codex — **Documented, not parsed**

| Scope | Path |
|---|---|
| User | `~/.codex/config.toml` |
| Project | `.codex/config.toml` |

**TOML, not JSON**, using `[mcp_servers.<name>]` tables.

MCP Doctor detects this configuration and reports it as unsupported (`MDC0011`).

*Source: `developers.openai.com/codex/mcp/`.*

## Cross-client differences that matter

These are the details most likely to trip up generic tooling.

### The `type` field has at least five spellings

| Spelling | Clients |
|---|---|
| absent | Claude Desktop, Zed, Windsurf, Gemini CLI |
| `stdio` / `http` / `sse` | VS Code, Cursor, Claude Code, Amazon Q |
| `streamableHttp` (camelCase) | Cline |
| `streamable-http` (kebab-case) | Roo Code, Continue |
| `stdio` required | Cursor |

A validator that hardcodes one spelling will report false errors on most real
configurations.

### The URL key differs

`url` for most clients, `serverUrl` for Windsurf, `httpUrl` for Gemini CLI — and in
Gemini CLI's case `url` and `httpUrl` select *different transports*.

### The root key differs

`mcpServers` for most, `servers` for VS Code's native format, `context_servers` for Zed.

### Project-level configuration

Supported by Claude Code (`.mcp.json`), VS Code (`.vscode/mcp.json`), Roo Code
(`.roo/mcp.json`), Amazon Q (`.amazonq/mcp.json`), Gemini CLI (`.gemini/settings.json`),
and Zed (`.zed/settings.json`).

Not documented for Cursor, Windsurf, or Cline.

### Interpolation syntax differs

`${VAR}` (Claude Code), `${env:NAME}` (Cursor, Windsurf, Roo Code), `$VAR` (Gemini CLI),
`${input:id}` (VS Code). MCP Doctor reports placeholders as a warning rather than
assuming any particular expansion.

## Known limitations

These are deliberate v1 scope decisions, not oversights.

- **YAML and TOML configurations are detected but not parsed.** Continue and Codex users
  get a clear "unsupported format" finding rather than silence.
- **Legacy HTTP+SSE is not probed.** It is deprecated upstream and the SDK's SSE client
  is a compatibility path.
- **WebSocket and socket transports are recognised but not probed.**
- **`IsProjectPath` cannot classify `.cursor/mcp.json`.** The global and project files
  share a name, and given only a path with no project root the two are indistinguishable.
  Ambiguous paths are left unclassified rather than guessed, because wrongly warning a
  user about their own configuration is worse than saying nothing.
- **Project-level Claude Code configuration under `~/.claude.json`** nests servers under
  `projects["<abs path>"]`. Only the top-level `mcpServers` is read, because resolving
  which project key applies requires knowing the working directory you intended.

## Dependencies

| Dependency | Version | Why |
|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` | `v1.8.0` | Protocol correctness. Stable v1, not experimental |
| `github.com/spf13/cobra` | `v1.10.2` | CLI parsing, help, and subcommand structure |

Everything else is the Go standard library. Two direct dependencies is a deliberate
choice: a diagnostic tool that a user installs to debug their setup should not itself
have a large supply chain.

The SDK is a Tier 1 MCP implementation per the official SDK documentation.