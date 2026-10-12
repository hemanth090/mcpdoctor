package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// TransportKind is how a client reaches a server.
type TransportKind string

const (
	// TransportStdio launches a subprocess and speaks newline-delimited JSON over its
	// stdin and stdout.
	TransportStdio TransportKind = "stdio"

	// TransportHTTP is the Streamable HTTP transport defined by the 2025-06-18 and
	// later revisions of the MCP specification.
	TransportHTTP TransportKind = "http"

	// TransportSSE is the legacy HTTP+SSE transport from protocol version 2024-11-05.
	// It is deprecated in the specification but still configured by some users, so it is
	// detected and reported rather than ignored.
	TransportSSE TransportKind = "sse"
)

// Scope distinguishes a per-user config from a per-project config.
type Scope string

const (
	// ScopeUser is a config for the current user, typically under their home directory.
	ScopeUser Scope = "user"

	// ScopeProject is a config committed to a repository and shared with a team.
	//
	// Project configs deserve extra scrutiny: checking one out means running code that
	// a stranger wrote. MCP Doctor never executes a project config without explicit
	// authorisation, and says so in its findings.
	ScopeProject Scope = "project"
)

// Client identifies an MCP client whose configuration this tool understands.
type Client string

const (
	ClientClaudeDesktop Client = "claude-desktop"
	ClientClaudeCode    Client = "claude-code"
	ClientVSCode        Client = "vscode"
	ClientCursor        Client = "cursor"
	ClientWindsurf      Client = "windsurf"
	ClientZed           Client = "zed"
	ClientCline         Client = "cline"
	ClientRooCode       Client = "roo-code"
	ClientAmazonQ       Client = "amazon-q"
	ClientGeminiCLI     Client = "gemini-cli"
	ClientCopilotCLI    Client = "copilot-cli"
	ClientContinue      Client = "continue"
	ClientCodex         Client = "codex"
)

// pathFunc computes a config path. Returning an empty string means "not applicable on
// this platform", which is how a Windows-only path is expressed without a build tag
// for every client.
type pathFunc func() string

// Adapter describes one client's configuration format.
//
// Every field here reflects a documented client format. Where clients disagree, the
// disagreement is modelled in data rather than hidden in conditionals, because the
// disagreements are the interesting part: the `type` field alone has at least five
// documented spellings across supported clients.
type Adapter struct {
	// Client is the stable identifier for this client.
	Client Client

	// DisplayName is the human-readable name shown in output.
	DisplayName string

	// Format describes the on-disk syntax. MCP Doctor supports JSON in v1; YAML and TOML
	// clients are detected and reported as unsupported rather than silently ignored.
	Format Format

	// ServersKey is the top-level JSON object holding server entries. It differs per
	// client: most use "mcpServers", VS Code uses "servers", and Zed uses
	// "context_servers".
	ServersKey string

	// ServersPath is the JSON path to the server map. It is more than one segment for
	// Claude Code's user scope, where servers live under projects/<path>/mcpServers.
	ServersPath []string

	// SupportedTypeValues lists the `type` spellings this client accepts, normalised to
	// a TransportKind. A value not in this list for the client is a warning, because it
	// means either a typo or a format we have not seen yet.
	SupportedTypeValues map[string]TransportKind

	// TransportFromKey maps a key's presence to a transport, for clients that have no
	// `type` field and infer the transport from which field is present. Gemini CLI is the
	// clearest example: command implies stdio, url implies SSE, and httpUrl implies
	// Streamable HTTP.
	TransportFromKey []TransportFromKey

	// URLKeys lists the keys that may carry a server URL, in priority order. Windsurf
	// uses "serverUrl" where most clients use "url".
	URLKeys []string

	// Users are the per-user config locations for this client on this platform.
	Users []pathFunc

	// Projects are the per-project config locations, relative to the project root.
	// Only clients documented to support a project-level MCP config appear here.
	Projects []string
}

// Format is a configuration file's on-disk syntax.
type Format string

const (
	// FormatJSON is the JSON object syntax used by most clients.
	FormatJSON Format = "json"

	// FormatYAML is the YAML syntax used by Continue. Detected but not parsed in v1.
	FormatYAML Format = "yaml"

	// FormatTOML is the TOML syntax used by OpenAI Codex. Detected but not parsed in v1.
	FormatTOML Format = "toml"
)

// TransportFromKey expresses "this key's presence selects that transport", which is how
// clients without a `type` field declare one.
type TransportFromKey struct {
	Key       string
	Transport TransportKind
}

// adapters is the registry of supported clients.
//
// It is a package-level value rather than a mutable registry: there is no runtime
// configuration that can add an adapter, which keeps the set of files this tool will
// read and the commands it may run fully determined by the binary itself.
var adapters = []Adapter{
	{
		Client:      ClientClaudeDesktop,
		DisplayName: "Claude Desktop",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		URLKeys:     []string{"url"},
		Users: []pathFunc{
			anyOS(func() string { return homePath("Library/Application Support/Claude/claude_desktop_config.json") }),
			anyOS(func() string { return appDataPath("Claude/claude_desktop_config.json") }),
			anyOS(func() string { return xdgConfigPath("Claude/claude_desktop_config.json") }),
		},
	},
	{
		Client:      ClientClaudeCode,
		DisplayName: "Claude Code",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		URLKeys:     []string{"url"},
		// Claude Code is the one client that documents three distinct spellings for the
		// same transport, including the streamable-http alias for http.
		SupportedTypeValues: map[string]TransportKind{
			"stdio":           TransportStdio,
			"http":            TransportHTTP,
			"streamable-http": TransportHTTP,
			"sse":             TransportSSE,
			"ws":              TransportHTTP, // WebSocket; recorded as HTTP-family
		},
		Users: []pathFunc{
			// The user-scope file also holds per-project servers under
			// projects["<abs path>"].mcpServers, which is why ServersPath is not just
			// the top-level key.
			func() string { return homePath(".claude.json") },
		},
		Projects: []string{".mcp.json"},
	},
	{
		Client:      ClientVSCode,
		DisplayName: "VS Code",
		Format:      FormatJSON,
		// VS Code uses "servers" in its own format, while the portable format it also
		// reads uses "mcpServers". Both are checked.
		ServersKey: "servers",
		URLKeys:    []string{"url"},
		SupportedTypeValues: map[string]TransportKind{
			"stdio": TransportStdio,
			"http":  TransportHTTP,
			"sse":   TransportSSE,
		},
		Users: []pathFunc{
			anyOS(func() string { return homePath("Library/Application Support/Code/User/mcp.json") }),
			anyOS(func() string { return appDataPath("Code/User/mcp.json") }),
			anyOS(func() string { return xdgConfigPath("Code/User/mcp.json") }),
		},
		Projects: []string{".vscode/mcp.json"},
	},
	{
		Client:              ClientCursor,
		DisplayName:         "Cursor",
		Format:              FormatJSON,
		ServersKey:          "mcpServers",
		URLKeys:             []string{"url"},
		SupportedTypeValues: map[string]TransportKind{"stdio": TransportStdio, "sse": TransportSSE, "http": TransportHTTP},
		Users:               []pathFunc{func() string { return homePath(".cursor/mcp.json") }},
		Projects:            []string{".cursor/mcp.json"},
	},
	{
		Client:      ClientWindsurf,
		DisplayName: "Windsurf",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		// Windsurf uses serverUrl rather than url, and documents no type field.
		URLKeys: []string{"serverUrl", "url"},
		TransportFromKey: []TransportFromKey{
			{Key: "command", Transport: TransportStdio},
			{Key: "serverUrl", Transport: TransportHTTP},
			{Key: "url", Transport: TransportHTTP},
		},
		Users: []pathFunc{
			func() string { return codeiumPath("windsurf/mcp_config.json") },
			func() string { return xdgConfigPath("devin/mcp_config.json") },
			func() string { return appDataPath("devin/mcp_config.json") },
		},
	},
	{
		Client:      ClientZed,
		DisplayName: "Zed",
		Format:      FormatJSON,
		ServersKey:  "context_servers",
		URLKeys:     []string{"url"},
		Users: []pathFunc{
			func() string { return xdgConfigPath("zed/settings.json") },
			func() string { return appDataPath("Zed/settings.json") },
		},
		Projects: []string{".zed/settings.json"},
	},
	{
		Client:      ClientCline,
		DisplayName: "Cline",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		URLKeys:     []string{"url"},
		// Cline spells the transport in camelCase where Roo Code uses kebab-case.
		SupportedTypeValues: map[string]TransportKind{
			"stdio":          TransportStdio,
			"streamableHttp": TransportHTTP,
			"sse":            TransportSSE,
		},
		Users: []pathFunc{
			func() string {
				if p := os.Getenv("CLINE_MCP_SETTINGS_PATH"); p != "" {
					return p
				}
				return homePath(".cline/data/settings/cline_mcp_settings.json")
			},
		},
	},
	{
		Client:      ClientRooCode,
		DisplayName: "Roo Code",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		URLKeys:     []string{"url"},
		// Roo Code uses the kebab-case spelling, the opposite of Cline's.
		SupportedTypeValues: map[string]TransportKind{
			"stdio":           TransportStdio,
			"streamable-http": TransportHTTP,
			"sse":             TransportSSE,
		},
		Users: []pathFunc{
			func() string { return vsCodeGlobalStoragePath("rooveterinaryinc.roo-cline/settings/mcp_settings.json") },
		},
		Projects: []string{".roo/mcp.json"},
	},
	{
		Client:              ClientAmazonQ,
		DisplayName:         "Amazon Q Developer CLI",
		Format:              FormatJSON,
		ServersKey:          "mcpServers",
		URLKeys:             []string{"url"},
		SupportedTypeValues: map[string]TransportKind{"http": TransportHTTP, "stdio": TransportStdio, "sse": TransportSSE},
		Users:               []pathFunc{func() string { return homePath(".aws/amazonq/mcp.json") }},
		Projects:            []string{".amazonq/mcp.json"},
	},
	{
		Client:      ClientGeminiCLI,
		DisplayName: "Gemini CLI",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		// Gemini CLI has no type field. The key that is present selects the transport,
		// and the three URL spellings mean three different things.
		URLKeys: []string{"httpUrl", "url"},
		TransportFromKey: []TransportFromKey{
			{Key: "command", Transport: TransportStdio},
			{Key: "httpUrl", Transport: TransportHTTP},
			{Key: "url", Transport: TransportSSE},
		},
		Users:    []pathFunc{func() string { return homePath(".gemini/settings.json") }},
		Projects: []string{".gemini/settings.json"},
	},
	{
		Client:      ClientCopilotCLI,
		DisplayName: "GitHub Copilot CLI",
		Format:      FormatJSON,
		ServersKey:  "mcpServers",
		URLKeys:     []string{"url"},
		Users: []pathFunc{
			func() string {
				if p := os.Getenv("COPILOT_HOME"); p != "" {
					return filepath.Join(p, "mcp-config.json")
				}
				return homePath(".copilot/mcp-config.json")
			},
		},
	},
	{
		Client:      ClientContinue,
		DisplayName: "Continue",
		// Continue stores MCP config as YAML, which MCP Doctor v1 detects but does not
		// parse. Reporting it as unsupported is honest; pretending to support it is not.
		Format:     FormatYAML,
		ServersKey: "mcpServers",
		URLKeys:    []string{"url"},
		Users: []pathFunc{
			func() string { return homePath(".continue/config.yaml") },
		},
		Projects: []string{".continue/mcpServers"},
	},
	{
		Client:      ClientCodex,
		DisplayName: "OpenAI Codex",
		Format:      FormatTOML,
		ServersKey:  "mcp_servers",
		URLKeys:     []string{"url"},
		Users: []pathFunc{
			func() string { return homePath(".codex/config.toml") },
		},
		Projects: []string{".codex/config.toml"},
	},
}

// Adapters returns the supported client adapters in a stable order.
//
// The returned slice is a copy of the registry contents; callers cannot mutate the
// tool's notion of which clients exist.
func Adapters() []Adapter {
	out := make([]Adapter, len(adapters))
	copy(out, adapters)
	return out
}

// AdapterFor returns the adapter for a client id.
func AdapterFor(c Client) (Adapter, bool) {
	for _, a := range adapters {
		if a.Client == c {
			return a, true
		}
	}
	return Adapter{}, false
}

// UserPaths returns the existing per-user config paths for a client on this platform.
//
// Only paths whose platform-specific function applies are considered, so a Windows path
// is never probed on Linux. Deduplication matters because several clients resolve to
// the same file on a given platform.
func (a Adapter) UserPaths() []string {
	var out []string
	seen := make(map[string]bool)
	for _, fn := range a.Users {
		if fn == nil {
			continue
		}
		p := fn()
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ProjectPaths returns the per-project config paths for a client under root.
//
// Only clients documented to support a project-level MCP config appear here. Returning
// paths for a client that does not support them would make `check` report a missing file
// for something the client will never read.
func (a Adapter) ProjectPaths(root string) []string {
	out := make([]string, 0, len(a.Projects))
	for _, rel := range a.Projects {
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
	}
	return out
}

// anyOS returns fn on every platform.
//
// Platform-specific path functions return "" when their environment variable is absent,
// which is how a Windows path declines to resolve on Linux without needing a build tag
// per adapter.
func anyOS(fn func() string) func() string { return fn }

// homePath resolves a path under the user's home directory.
//
// A missing home directory yields an empty string rather than a relative path, because
// a relative path would silently probe the current working directory and read the wrong
// file.
func homePath(rel string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, filepath.FromSlash(rel))
}

// appDataPath resolves a path under %APPDATA% on Windows.
//
// On other platforms APPDATA is consulted anyway: Amazon Q and Windsurf are documented
// with Windows paths, and a user migrating a config across machines should not silently
// lose their diagnostics.
func appDataPath(rel string) string {
	if base := os.Getenv("APPDATA"); base != "" {
		return filepath.Join(base, filepath.FromSlash(rel))
	}
	return ""
}

// xdgConfigPath resolves a path under XDG_CONFIG_HOME, falling back to ~/.config.
//
// The fallback matches the documented behaviour of Claude Desktop and Windsurf on
// Linux.
func xdgConfigPath(rel string) string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, filepath.FromSlash(rel))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", filepath.FromSlash(rel))
	}
	return ""
}

// codeiumPath resolves a path under the Codeium/Windsurf data directory.
func codeiumPath(rel string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".codeium", filepath.FromSlash(rel))
	}
	return ""
}

// vsCodeGlobalStoragePath resolves an extension's globalStorage settings file.
//
// The publisher slug is part of the path and has changed across releases, which is why
// the Roo Code entry documents medium confidence. The path is reported as a candidate
// for inspection rather than treated as authoritative.
func vsCodeGlobalStoragePath(rel string) string {
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, "Library/Application Support/Code/User/globalStorage", filepath.FromSlash(rel))
		}
	case "windows":
		if base := os.Getenv("APPDATA"); base != "" {
			return filepath.Join(base, "Code/User/globalStorage", filepath.FromSlash(rel))
		}
	default:
		if base := xdgConfigPath(""); base != "" {
			return filepath.Join(base, "Code/User/globalStorage", filepath.FromSlash(rel))
		}
	}
	return ""
}

// Source is a candidate configuration file discovered on disk.
//
// Discovery reports what could be read; it does not read. Keeping the two apart is what
// lets `check` explain "you have three clients installed and none are configured"
// without having parsed anything.
type Source struct {
	// Client is the client this config belongs to.
	Client Client

	// DisplayName is the client's human-readable name.
	DisplayName string

	// Path is the absolute path to the config file.
	Path string

	// Scope is whether this is a user or project config.
	Scope Scope

	// Format is the file's syntax.
	Format Format

	// Exists reports whether the file was present at discovery time.
	Exists bool
}

// String renders the source for display.
func (s Source) String() string {
	return fmt.Sprintf("%s (%s, %s)", s.DisplayName, s.Scope, s.Path)
}

// DescribePaths resolves the candidate config paths for every supported client.
//
// projectRoot may be empty, in which case project-level configs are skipped. The result
// is sorted by path so that output does not depend on registry order, and duplicate
// paths are collapsed: several clients legitimately read the same file, and reporting it
// three times would be noise.
func DescribePaths(projectRoot string) []Source {
	var out []Source
	seen := make(map[string]bool)

	add := func(a Adapter, path string, scope Scope) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, Source{
			Client:      a.Client,
			DisplayName: a.DisplayName,
			Path:        path,
			Scope:       scope,
			Format:      a.Format,
		})
	}

	for _, a := range adapters {
		for _, p := range a.UserPaths() {
			add(a, p, ScopeUser)
		}
		if projectRoot != "" {
			for _, p := range a.ProjectPaths(projectRoot) {
				add(a, p, ScopeProject)
			}
		}
	}

	sortSources(out)
	return out
}

// sortSources orders sources by path for deterministic output.
func sortSources(s []Source) {
	sort.Slice(s, func(i, j int) bool { return s[i].Path < s[j].Path })
}

// IsProjectPath reports whether a path looks like a project-scoped config.
//
// The check is on the directory layout rather than the registry because a user can pass
// --config with any path, and the distinction between "this file came from a repository
// someone else wrote" and "this is my own machine config" changes how much caution the
// output deserves.
func IsProjectPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	for _, marker := range []string{"/.mcp.json", "/.vscode/", "/.cursor/", "/.zed/", "/.roo/", "/.amazonq/", "/.gemini/", "/.continue/"} {
		if strings.HasSuffix(clean, marker) || strings.Contains(clean, marker) {
			return true
		}
	}
	return false
}
