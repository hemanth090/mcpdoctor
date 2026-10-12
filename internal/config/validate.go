package config

import (
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
)

// Server is one parsed MCP server entry, normalised across client formats.
//
// Normalisation happens once, here, so that the validation rules do not each have to
// know that Windsurf calls it serverUrl, that Gemini CLI selects a transport by key
// presence, or that four different spellings of Streamable HTTP all mean the same thing.
type Server struct {
	// Name is the server's key in the config.
	Name string

	// Pos is where the entry begins in the source file.
	Pos Position

	// Transport is the resolved transport kind. It is TransportStdio or TransportHTTP
	// unless the config was ambiguous, in which case Resolution records why.
	Transport TransportKind

	// RawType is the value of the client's type field, verbatim, if one was present.
	RawType string

	// TypeRecognized reports whether RawType was a value this client documents. A
	// recognised type is authoritative; an unrecognised one is almost always a typo, and
	// must be surfaced even when the transport could still be inferred from the fields.
	TypeRecognized bool

	// Command, Args, Cwd, Env, Headers, URL hold the raw configured values.
	Command string
	Args    []string
	Cwd     string
	Env     map[string]string
	Headers map[string]string
	URL     string

	// Disabled reports whether the client will not start this server.
	Disabled bool

	// TimeoutSeconds is the configured per-request timeout, if the client supports one.
	TimeoutSeconds float64

	// Raw holds the original JSON object for this entry, including keys that
	// normalisation did not interpret. Rules consult it to distinguish "field is an
	// array" from "field is a string", which normalisation alone would collapse.
	Raw map[string]any

	// Resolution explains, in one line, how Transport was determined. It is shown to the
	// user because "why did it think this was HTTP?" is a fair question when a client
	// has five ways to express the same thing.
	Resolution string
}

// serverKeys is the set of keys MCP Doctor understands inside a server entry.
//
// Anything outside this set is reported as an unknown field. That is a warning rather
// than an error on purpose: clients add keys over time, and a tool that treats every
// unfamiliar key as broken stops being useful the moment a client ships.
var serverKeys = map[string]bool{
	"command": true, "args": true, "env": true, "cwd": true, "url": true,
	"httpUrl": true, "serverUrl": true, "type": true, "headers": true,
	"disabled": true, "timeout": true, "envFile": true, "sandboxEnabled": true,
	"autoApprove": true, "alwaysAllow": true, "disabledTools": true,
	"dev": true, "watchPaths": true, "oauth": true, "auth": true, "dev.debug": true,
	"trust": true, "includeTools": true, "excludeTools": true, "authProviderType": true,
	"targetAudience": true, "targetServiceAccount": true, "headersHelper": true,
	"alwaysLoad": true,
}

// ParseServers extracts the server entries from a parsed config document.
//
// It returns the servers plus any findings raised while locating the servers map. A
// config with no servers map is not an error: plenty of clients ship with an empty
// configuration, and reporting that as broken would be noise.
func ParseServers(a Adapter, node *Node) ([]Server, []diagnose.Finding) {
	if node == nil {
		return nil, nil
	}

	root, ok := node.Root.Obj()
	if !ok {
		return nil, []diagnose.Finding{{
			Code:     diagnose.CodeConfigJSONType,
			Severity: diagnose.SeverityError,
			Target:   a.DisplayName,
			Message:  "the top level of the file is not a JSON object",
			Detail:   "An MCP client configuration must be a JSON object with a server map at the top level.",
			Remedy:   "Fix the file so its outermost braces enclose an object.",
		}}
	}

	container, found := findServersContainer(a, root)
	if !found {
		return nil, nil
	}

	serversObj, ok := container.Value.(map[string]any)
	if !ok {
		return nil, []diagnose.Finding{{
			Code:     diagnose.CodeServersNotObject,
			Severity: diagnose.SeverityError,
			Target:   a.DisplayName,
			Location: Location("", container.Pos),
			Message:  "\"" + strings.Join(a.ServersPath, ".") + "\" is not an object",
			Detail:   "Each entry under the server map must be an object describing one server.",
			Remedy:   "Replace the value with an object of server-name to server-definition.",
		}}
	}

	var findings []diagnose.Finding
	var servers []Server

	names := make([]string, 0, len(serversObj))
	for name := range serversObj {
		names = append(names, name)
	}
	// Map iteration order is random; sorting makes the report reproducible and makes
	// test assertions possible without depending on luck.
	sort.Strings(names)

	for _, name := range names {
		entry, ok := serversObj[name].(map[string]any)
		if !ok {
			findings = append(findings, diagnose.Finding{
				Code:     diagnose.CodeServerNotObject,
				Severity: diagnose.SeverityError,
				Target:   name,
				Location: Location("", container.Pos),
				Message:  "server entry is not an object",
				Detail:   "Each server must be an object with a command (stdio) or a url (HTTP).",
				Remedy:   "Replace this entry with an object, or remove it.",
			})
			continue
		}
		servers = append(servers, normaliseServer(a, name, entry, container.Pos))
	}

	return servers, findings
}

// findServersContainer locates the object that holds server entries.
//
// The search walks a ServersPath when the client needs one (Claude Code's per-project
// scope) and otherwise falls back to the ServersKey, trying the alternates some clients
// also accept.
func findServersContainer(a Adapter, root map[string]any) (Located, bool) {
	keys := a.ServersPath
	if len(keys) == 0 {
		keys = []string{a.ServersKey}
	}
	// VS Code documents both a "servers" key and the portable "mcpServers" key.
	if a.Client == ClientVSCode {
		keys = append(keys, "mcpServers")
	}

	for _, key := range keys {
		v, present := root[key]
		if !present {
			continue
		}
		return Located{Value: v, Pos: Position{Line: 1, Col: 1}}, true
	}
	return Located{}, false
}

// normaliseServer converts one raw entry into a [Server].
//
// It is deliberately total: it never returns an error, because a partially understood
// entry is still worth reporting on. Anything it could not interpret stays at its zero
// value, and the validation rules notice what is missing.
func normaliseServer(a Adapter, name string, entry map[string]any, pos Position) Server {
	s := Server{Name: name, Pos: pos, Raw: entry}

	if c, ok := entry["command"].(string); ok {
		s.Command = c
	}
	if arr, ok := entry["args"].([]any); ok {
		for _, v := range arr {
			s.Args = append(s.Args, toString(v))
		}
	}
	if cwd, ok := entry["cwd"].(string); ok {
		s.Cwd = cwd
	}
	s.Env = toStringMap(entry["env"])
	s.Headers = toStringMap(entry["headers"])

	if t, ok := entry["type"].(string); ok {
		s.RawType = t
	}
	if b, ok := entry["disabled"].(bool); ok {
		s.Disabled = b
	}
	if n, ok := entry["timeout"].(float64); ok {
		s.TimeoutSeconds = n
	}

	// Pick the URL using the client's documented key priority.
	for _, key := range a.URLKeys {
		if u, ok := entry[key].(string); ok && u != "" {
			s.URL = u
			break
		}
	}

	s.Transport, s.Resolution = resolveTransport(a, entry, &s)
	return s
}

// resolveTransport determines the transport from whichever signal the client provides.
//
// The order matters and follows what each client documents: an explicit, recognised
// type field wins; otherwise the transport is inferred from which key is present. A
// type field that this client does not recognise is a warning rather than a silent
// fallback, because a typo in `type` is one of the more common real failures.
func resolveTransport(a Adapter, entry map[string]any, s *Server) (TransportKind, string) {
	if s.RawType != "" {
		if kind, ok := a.SupportedTypeValues[s.RawType]; ok {
			s.TypeRecognized = true
			return kind, "type: " + s.RawType
		}
		// Unrecognised type: fall through to inference so the rest of the diagnosis is
		// still useful, but remember that the type field itself was wrong. Without this
		// flag a typo in `type` would be silently masked by a successful inference, which
		// is the exact failure this rule exists to catch.
		if kind, res := inferTransport(a, entry, *s); kind != "" {
			s.TypeRecognized = false
			return kind, "type " + strconv.Quote(s.RawType) + " not recognised by this client; " + res
		}
		s.TypeRecognized = false
		return "", "type " + strconv.Quote(s.RawType) + " not recognised and no other signal present"
	}

	if kind, res := inferTransport(a, entry, *s); kind != "" {
		s.TypeRecognized = true
		return kind, res
	}
	s.TypeRecognized = true
	return "", ""
}

// inferTransport selects a transport from the fields that are present.
func inferTransport(a Adapter, entry map[string]any, s Server) (TransportKind, string) {
	// An explicit key-to-transport table, for clients that document which key implies
	// which transport. Gemini CLI is the motivating case: command means stdio, httpUrl
	// means Streamable HTTP, and bare url means the legacy SSE transport.
	for _, rule := range a.TransportFromKey {
		if _, present := entry[rule.Key]; present {
			if rule.Key == "command" && s.Command == "" {
				continue // present but empty; the command rules will explain it
			}
			return rule.Transport, "inferred from \"" + rule.Key + "\""
		}
	}
	if s.Command != "" {
		return TransportStdio, "inferred from \"command\""
	}
	if s.URL != "" {
		return TransportHTTP, "inferred from \"url\""
	}
	return "", ""
}

// toString renders a JSON scalar as a string, which is how a malformed entry's fields
// are preserved for display rather than dropped.
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return formatFloat(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return ""
	}
}

// toStringMap converts a JSON object to a string map, skipping non-string values.
func toStringMap(v any) map[string]string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(obj))
	for k, val := range obj {
		out[k] = toString(val)
	}
	return out
}

// formatFloat renders a float without a trailing ".0" for whole numbers, so a timeout of
// 30 reads as "30" rather than "30.0".
func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Rule is a validation function applied to a normalised server.
//
// Rules return findings rather than mutating the server, so they compose and can be run
// in any order.
type Rule func(a Adapter, s Server, r *redact.Redactor) []diagnose.Finding

// rules is the ordered rule set applied to every server.
//
// Order is meaningful: transport validity is checked before command and URL details, so
// a server with no transport at all produces one clear finding rather than a pile of
// downstream complaints about missing command and missing url.
var rules = []Rule{
	ruleServerName,
	ruleTransport,
	ruleCommand,
	ruleArgs,
	ruleEnv,
	ruleCwd,
	ruleURL,
	ruleHeaders,
	ruleDisabled,
	ruleTimeout,
	ruleUnknownFields,
	ruleSecrets,
}

// ValidateServer runs every rule against one server and returns the findings.
func ValidateServer(a Adapter, s Server, r *redact.Redactor) []diagnose.Finding {
	var findings []diagnose.Finding
	for _, rule := range rules {
		findings = append(findings, rule(a, s, r)...)
	}
	return findings
}

// ValidateAll runs every rule against every server, in a deterministic order.
func ValidateAll(a Adapter, servers []Server, r *redact.Redactor) []diagnose.Finding {
	var findings []diagnose.Finding
	for _, s := range servers {
		findings = append(findings, ValidateServer(a, s, r)...)
	}
	return findings
}

// at builds a finding for a server with a location already applied.
func at(s Server, f diagnose.Finding) diagnose.Finding {
	if f.Target == "" {
		f.Target = s.Name
	}
	return f
}

// ruleServerName rejects names that will render badly in a client UI or collide with
// config lookups.
func ruleServerName(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	if strings.TrimSpace(s.Name) == "" {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeServerNameInvalid,
			Severity: diagnose.SeverityError,
			Message:  "server name is empty",
			Detail:   "An empty name cannot be selected in the client UI and cannot be addressed on the command line.",
			Remedy:   "Give the server a name.",
		})}
	}
	if strings.TrimSpace(s.Name) != s.Name {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeServerNameInvalid,
			Severity: diagnose.SeverityWarning,
			Message:  "server name has leading or trailing whitespace",
			Detail:   "The name is used verbatim, so the server appears with invisible padding that is hard to type or select.",
			Remedy:   "Remove the surrounding whitespace from the name.",
			Example:  "\"filesystem\": { }",
		})}
	}
	return nil
}

// ruleTransport checks that the entry describes exactly one usable transport.
func ruleTransport(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	hasCommand := strings.TrimSpace(s.Command) != ""
	hasURL := strings.TrimSpace(s.URL) != ""

	switch {
	case !hasCommand && !hasURL:
		severity := diagnose.SeverityError
		detail := "There is nothing for the client to connect to, so this server will fail to start."
		if s.RawType != "" {
			detail = "The entry declares a transport type but no matching endpoint. This is the documented failure mode for a url entry read as stdio."
		}
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeServerNoTransport,
			Severity: severity,
			Message:  "server defines neither a command nor a url",
			Detail:   detail,
			Remedy:   "Add \"command\" for a local process, or \"url\" for an HTTP server.",
			Example:  "\"filesystem\": { \"command\": \"npx\", \"args\": [\"-y\", \"@modelcontextprotocol/server-filesystem\"] }",
		})}
	case hasCommand && hasURL:
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeServerBothTransports,
			Severity: diagnose.SeverityError,
			Message:  "server defines both a command and a url",
			Detail:   "A server is either launched as a process or reached over HTTP, not both. The client has to choose, and the result is rarely what you intended.",
			Remedy:   "Keep whichever one you meant and delete the other.",
		})}
	}

	if s.RawType != "" && !s.TypeRecognized {
		severity := diagnose.SeverityError
		if s.Transport != "" {
			// The transport was still inferable, so the entry may work by accident. That
			// is a warning, not an error: the user should fix the type, but nothing is
			// currently broken.
			severity = diagnose.SeverityWarning
		}
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeTypeMismatch,
			Severity: severity,
			Message:  "transport type " + strconv.Quote(s.RawType) + " is not recognised by " + a.DisplayName,
			Detail:   resolutionDetail(a, s),
			Remedy:   "Use one of the documented type values for this client, or remove the field so the transport is inferred from the command or url.",
			Example:  "\"type\": \"" + exampleTypeFor(a, s) + "\"",
		})}
	}
	return nil
}

// resolutionDetail explains how the transport was determined, for the case where the
// user and the tool disagree about what a config means.
func resolutionDetail(a Adapter, s Server) string {
	if s.Resolution == "" {
		return "The transport could not be determined from this entry."
	}
	return "Transport resolution: " + s.Resolution + "."
}

// exampleTypeFor suggests the documented type value that matches what the entry already
// contains, which is usually what the user meant.
func exampleTypeFor(a Adapter, s Server) string {
	want := TransportHTTP
	if strings.TrimSpace(s.Command) != "" {
		want = TransportStdio
	}
	// Prefer a value this client actually documents, so the example is copyable.
	names := make([]string, 0, len(a.SupportedTypeValues))
	for name := range a.SupportedTypeValues {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if a.SupportedTypeValues[name] == want {
			return name
		}
	}
	return string(want)
}

// ruleCommand validates the command field and, where it is a bare name, whether it
// resolves on PATH.
func ruleCommand(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	if s.Transport == TransportHTTP {
		return nil
	}
	if strings.TrimSpace(s.Command) == "" {
		return nil // reported by ruleTransport
	}

	cmd := strings.TrimSpace(s.Command)
	var findings []diagnose.Finding

	// A pasted shell line is a common mistake: the user copies
	// "npx -y @scope/pkg" into the command field, where the client will treat the whole
	// thing as one executable name.
	if containsShellSeparator(cmd) {
		findings = append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeCommandContainsSeparator,
			Severity: diagnose.SeverityError,
			Message:  "command contains shell syntax or a path separator",
			Detail:   "The command field is a single executable, not a shell line. Arguments belong in the separate \"args\" array.",
			Remedy:   "Move everything after the executable into \"args\".",
			Example:  "\"command\": \"npx\", \"args\": [\"-y\", \"@scope/pkg\"]",
		}))
		return findings
	}

	if strings.ContainsAny(cmd, "\x00\n\r") {
		findings = append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeCommandEmpty,
			Severity: diagnose.SeverityError,
			Message:  "command contains a control character",
			Detail:   "A NUL or newline in an executable name indicates a malformed or hand-edited config, and cannot be executed safely.",
			Remedy:   "Replace the command with a plain executable name or an absolute path.",
		}))
		return findings
	}

	if isAbsolutePath(cmd) {
		checkAbsoluteCommand(s, cmd, &findings)
		return findings
	}

	// A bare name is resolved against PATH, which means the server can change meaning
	// depending on the environment the client happens to run in. That is usually what the
	// user wants, so it is a warning, not an error.
	if _, err := exec.LookPath(cmd); err != nil {
		findings = append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeCommandNotFound,
			Severity: diagnose.SeverityError,
			Message:  "command " + strconv.Quote(cmd) + " was not found on PATH",
			Detail:   "The client resolves a bare command name against the PATH it inherits. If the executable is installed somewhere not on PATH, or is installed under a different name, the server cannot start.",
			Remedy:   "Install the executable so it is on PATH, or use its absolute path.",
			Example:  "\"command\": \"/usr/local/bin/" + cmd + "\"",
		}))
	}
	return findings
}

// checkAbsoluteCommand validates a fully-qualified command path.
func checkAbsoluteCommand(s Server, cmd string, findings *[]diagnose.Finding) {
	clean := filepath.Clean(cmd)
	if !fileExists(clean) {
		*findings = append(*findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeCommandNotFound,
			Severity: diagnose.SeverityError,
			Message:  "command path does not exist: " + cmd,
			Detail:   "An absolute path does not fall back to PATH, so the server cannot start if the file is missing. A moved or per-machine install is the usual cause.",
			Remedy:   "Correct the path, or switch to a bare command name to use PATH lookup.",
		}))
		return
	}
	if !isExecutableFile(clean) {
		*findings = append(*findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeCommandNotExecutable,
			Severity: diagnose.SeverityError,
			Message:  "command path is not an executable file: " + cmd,
			Detail:   "The file exists but cannot be run: it is a directory, or it lacks the execute permission bit.",
			Remedy:   "Point the command at the executable, and add the execute permission if the file is a script.",
		}))
	}
}

// ruleArgs validates the args array.
func ruleArgs(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	raw, present := rawField(s, "args")
	if !present {
		return nil
	}
	if _, ok := raw.([]any); !ok {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeArgsInvalid,
			Severity: diagnose.SeverityError,
			Message:  "\"args\" is not an array",
			Detail:   "Arguments must be an array of strings. A single string is a common mistake, and clients reject it or treat it as one argument.",
			Remedy:   "Wrap each argument in its own quoted string.",
			Example:  "\"args\": [\"-y\", \"@scope/pkg\"]",
		})}
	}

	var findings []diagnose.Finding
	for i, arg := range s.Args {
		if arg == "" {
			findings = append(findings, at(s, diagnose.Finding{
				Code:     diagnose.CodeArgEmpty,
				Severity: diagnose.SeverityWarning,
				Message:  "argument " + strconv.Itoa(i) + " is an empty string",
				Detail:   "An empty argument is almost never intended, and usually comes from a copy-paste that lost a value between quotes.",
				Remedy:   "Remove the empty argument, or fill in the missing value.",
			}))
			continue
		}
		if containsShellMeta(arg) {
			findings = append(findings, at(s, diagnose.Finding{
				Code:     diagnose.CodeArgsShellMetacharacter,
				Severity: diagnose.SeverityWarning,
				Message:  "argument " + strconv.Itoa(i) + " contains shell metacharacters: " + strconv.Quote(arg),
				Detail:   "Arguments are passed directly to the process, so metacharacters are not interpreted and the server receives the literal characters. That is correct behaviour, but it is also the signature of a command line pasted where an argument array was expected.",
				Remedy:   "If the server should receive the literal characters, no change is needed. If you expected shell behaviour, restructure the command.",
			}))
		}
	}
	return findings
}

// ruleEnv validates the env object and registers its values with the redactor.
func ruleEnv(a Adapter, s Server, r *redact.Redactor) []diagnose.Finding {
	raw, present := rawField(s, "env")
	if !present {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeEnvInvalid,
			Severity: diagnose.SeverityError,
			Message:  "\"env\" is not an object",
			Detail:   "Environment variables must be an object mapping a name to a string value.",
			Remedy:   "Replace the value with an object such as {\"API_KEY\": \"value\"}.",
		})}
	}

	var findings []diagnose.Finding
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		value, _ := s.Env[name]

		// Register the value before anything can be printed. This is the single point
		// where config secrets enter the redaction system.
		r.AddEnv(name, value)

		if !validEnvName(name) {
			findings = append(findings, at(s, diagnose.Finding{
				Code:     diagnose.CodeEnvNameInvalid,
				Severity: diagnose.SeverityError,
				Message:  "environment variable name " + strconv.Quote(name) + " is not valid",
				Detail:   "A name must start with a letter or underscore and contain only letters, digits, and underscores. Clients drop invalid names silently, so the server starts without the variable it needed.",
				Remedy:   "Rename the variable to a valid identifier.",
				Example: "\"" + strings.Map(func(r rune) rune {
					if r == '-' || r == '.' || r == ' ' {
						return '_'
					}
					return r
				}, name) + "\": \"" + redact.Placeholder + "\"",
			}))
			continue
		}

		if strings.Contains(value, "${") || (strings.HasPrefix(value, "$") && len(value) > 1) {
			findings = append(findings, at(s, diagnose.Finding{
				Code:     diagnose.CodeEnvInterpolation,
				Severity: diagnose.SeverityWarning,
				Message:  "env value for " + strconv.Quote(name) + " contains a variable placeholder",
				Detail:   "Most clients pass environment values through literally without expanding them, so the server receives the placeholder text instead of the value it names. Client support for interpolation varies and is not consistent across clients.",
				Remedy:   "Set the value literally, or confirm your client documents interpolation for this field before relying on it.",
			}))
		}
	}
	return findings
}

// ruleCwd validates the working directory.
func ruleCwd(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	if s.Cwd == "" {
		return nil
	}
	raw, present := rawField(s, "cwd")
	if !present {
		return nil
	}
	if _, ok := raw.(string); !ok {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeCwdInvalid,
			Severity: diagnose.SeverityError,
			Message:  "\"cwd\" is not a string",
			Detail:   "The working directory must be a single path.",
			Remedy:   "Set it to a quoted path string.",
		})}
	}
	if !dirExists(s.Cwd) {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeCwdInvalid,
			Severity: diagnose.SeverityError,
			Message:  "working directory does not exist: " + s.Cwd,
			Detail:   "A relative cwd is resolved against the directory the client happens to launch from, so a relative path can work in one project and fail in another. An absolute path is more predictable.",
			Remedy:   "Create the directory, or correct the path. Prefer an absolute path.",
		})}
	}
	return nil
}

// ruleURL validates the remote endpoint.
func ruleURL(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	if s.Transport == TransportStdio {
		return nil
	}
	if strings.TrimSpace(s.URL) == "" {
		return nil // reported by ruleTransport
	}

	raw := s.URL
	u, err := url.Parse(raw)
	if err != nil {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeURLInvalid,
			Severity: diagnose.SeverityError,
			Message:  "url is not a valid URL: " + raw,
			Detail:   "The endpoint could not be parsed, so the client cannot connect.",
			Remedy:   "Use an absolute URL such as https://example.com/mcp.",
		})}
	}

	var findings []diagnose.Finding

	if u.Scheme == "" {
		return append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeURLInvalid,
			Severity: diagnose.SeverityError,
			Message:  "url has no scheme: " + raw,
			Detail:   "A relative URL is meaningless to a remote MCP client.",
			Remedy:   "Include the scheme, for example https://example.com/mcp.",
		}))
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeURLSchemeUnsupported,
			Severity: diagnose.SeverityError,
			Message:  "unsupported URL scheme " + strconv.Quote(u.Scheme),
			Detail:   "MCP Doctor speaks http and https. Any other scheme is not an MCP endpoint and probing it could reach an unexpected service.",
			Remedy:   "Use https, or http for a local server.",
		}))
	}

	if u.User != nil {
		findings = append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeURLCredentials,
			Severity: diagnose.SeverityWarning,
			Message:  "url embeds a username or password",
			Detail:   "Credentials in a URL are easy to miss when the config is shared or committed, and they tend to end up in logs and shell history.",
			Remedy:   "Move the credential into an Authorization header or an environment variable.",
			Example:  "\"headers\": { \"Authorization\": \"Bearer " + redact.Placeholder + "\" }",
		}))
	}

	// Plain HTTP to anything that is not loopback sends credentials and tool traffic in
	// cleartext, which is a real risk rather than a style preference.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		findings = append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeURLInsecure,
			Severity: diagnose.SeverityError,
			Message:  "plain http used for a non-local endpoint: " + u.Hostname(),
			Detail:   "Unencrypted HTTP exposes any Authorization header and all tool traffic to anyone on the network path. Loopback endpoints are exempt because they never leave the machine.",
			Remedy:   "Use https for any endpoint that is not on this machine.",
		}))
	}

	if u.Path == "" {
		findings = append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeURLInvalid,
			Severity: diagnose.SeverityWarning,
			Message:  "url has no path component",
			Detail:   "MCP endpoints conventionally serve a path such as /mcp. A bare origin usually means the path was omitted.",
			Remedy:   "Confirm the endpoint path with the server, and include it if one is required.",
		}))
	}

	return findings
}

// ruleHeaders validates the headers object.
func ruleHeaders(a Adapter, s Server, r *redact.Redactor) []diagnose.Finding {
	raw, present := rawField(s, "headers")
	if !present {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeHeadersInvalid,
			Severity: diagnose.SeverityError,
			Message:  "\"headers\" is not an object",
			Detail:   "Headers must be an object mapping a header name to its value.",
			Remedy:   "Replace the value with an object such as {\"Authorization\": \"Bearer ...\"}.",
		})}
	}

	var findings []diagnose.Finding
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		value := s.Headers[name]
		// Header values carry the same credentials as env values and are registered
		// with the redactor for the same reason.
		r.AddEnv("HEADER_"+strings.ToUpper(name), value)

		if strings.TrimSpace(value) == "" {
			findings = append(findings, at(s, diagnose.Finding{
				Code:     diagnose.CodeHeadersInvalid,
				Severity: diagnose.SeverityWarning,
				Message:  "header " + strconv.Quote(name) + " has an empty value",
				Detail:   "An empty header is sent as present but blank, which is different from omitting it and often causes a confusing authentication failure.",
				Remedy:   "Remove the header, or give it a value.",
			}))
		}
		if strings.Contains(value, "${") {
			findings = append(findings, at(s, diagnose.Finding{
				Code:     diagnose.CodeEnvInterpolation,
				Severity: diagnose.SeverityWarning,
				Message:  "header " + strconv.Quote(name) + " contains a variable placeholder",
				Detail:   "Not every client expands placeholders in header values, and support differs between clients, so the server may receive the literal text.",
				Remedy:   "Set the value literally unless your client's documentation confirms interpolation for headers.",
			}))
		}
	}
	return findings
}

// ruleDisabled reports servers the client will not start.
func ruleDisabled(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	if !s.Disabled {
		return nil
	}
	return []diagnose.Finding{at(s, diagnose.Finding{
		Code:     diagnose.CodeServerDisabled,
		Severity: diagnose.SeverityInfo,
		Message:  "server is marked disabled",
		Detail:   "The client will not start this server, so any problem reported for it has no effect until it is enabled.",
		Remedy:   "Enable the server if you expected it to be available.",
	})}
}

// ruleTimeout validates the configured timeout.
func ruleTimeout(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	raw, present := rawField(s, "timeout")
	if !present {
		return nil
	}
	n, ok := raw.(float64)
	if !ok {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeTimeoutInvalid,
			Severity: diagnose.SeverityError,
			Message:  "\"timeout\" is not a number",
			Detail:   "Clients express timeouts as a number of seconds, though the unit differs between clients.",
			Remedy:   "Use a positive number.",
			Example:  "\"timeout\": 60",
		})}
	}
	if n <= 0 {
		return []diagnose.Finding{at(s, diagnose.Finding{
			Code:     diagnose.CodeTimeoutInvalid,
			Severity: diagnose.SeverityError,
			Message:  "timeout must be greater than zero",
			Detail:   "A zero or negative timeout either means immediate failure in most clients, or is rejected outright.",
			Remedy:   "Use a positive number of seconds.",
		})}
	}
	return nil
}

// ruleUnknownFields reports keys MCP Doctor does not recognise.
//
// This is the mechanism that surfaces client format drift. It is a warning, because a
// client adding a key we have not read about is far more likely than a user having made
// a mistake.
func ruleUnknownFields(a Adapter, s Server, _ *redact.Redactor) []diagnose.Finding {
	var findings []diagnose.Finding
	for name := range s.Raw {
		if name == "" || serverKeys[name] {
			continue
		}
		findings = append(findings, at(s, diagnose.Finding{
			Code:     diagnose.CodeUnknownField,
			Severity: diagnose.SeverityWarning,
			Message:  "unrecognised field " + strconv.Quote(name) + " in server entry",
			Detail:   "MCP Doctor does not know this field, so it cannot validate it. That usually means " + a.DisplayName + " documents a field this tool has not been taught yet; it is not necessarily an error.",
			Remedy:   "If the field is real, it is safe to leave. If it is a typo, correct it.",
		}))
	}
	return findings
}

// ruleSecrets warns about credentials committed to a configuration file.
func ruleSecrets(a Adapter, s Server, r *redact.Redactor) []diagnose.Finding {
	var findings []diagnose.Finding

	// check reports a credential-shaped value. The value itself is never included in the
	// finding: naming the variable is what the user needs, disclosing the value would be the
	// exact harm this rule exists to prevent.
	check := func(kind, name string, m map[string]string) {
		if looksSecretValue(name, m[name]) {
			findings = append(findings, at(s, diagnose.Finding{
				Code:     diagnose.CodeSecretInConfig,
				Severity: diagnose.SeverityWarning,
				Message:  kind + " " + strconv.Quote(name) + " appears to hold a credential",
				Detail:   "The value itself is never printed. If this configuration file is committed to a repository, the credential is committed with it, and repository history retains it even after a later commit removes it.",
				Remedy:   "Move the credential to an environment variable you set outside the file, or to the client's secret store, and rotate it if it was ever committed.",
				Example:  "\"" + name + "\": \"" + redact.Placeholder + "\"",
			}))
		}
	}

	for _, name := range sortedKeys(s.Env) {
		check("environment variable", name, s.Env)
	}
	for _, name := range sortedKeys(s.Headers) {
		check("header", name, s.Headers)
	}
	return findings
}

// looksSecretValue is a thin wrapper so the rule reads clearly at its call site.
func looksSecretValue(name, value string) bool { return redact.LooksSecret(name, value) }

// rawField returns the raw JSON value of a field.
//
// Rules need the raw value, not the normalised one, because "args is an array" and "args
// is a string" are different diagnoses and normalisation collapses that difference.
// The normalised Server carries raw fields alongside the parsed ones for this purpose.
func rawField(s Server, name string) (any, bool) {
	raw, ok := s.Raw[name]
	return raw, ok
}

// sortedKeys returns a map's keys in sorted order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shellMetaChars are the characters that indicate shell syntax rather than a literal
// argument.
const shellMetaChars = ";|&$`<>\n"

// containsShellMeta reports whether a string contains shell metacharacters.
func containsShellMeta(s string) bool {
	return strings.ContainsAny(s, shellMetaChars)
}

// containsShellSeparator reports whether a command field contains something that only
// makes sense in a shell line.
func containsShellSeparator(s string) bool {
	if strings.ContainsAny(s, ";|&`$") {
		return true
	}
	// A space-separated word list is a pasted command line. A legitimate path can
	// contain a space, but a bare executable name with spaces in it is far more often a
	// paste error, so it is reported and the user can judge.
	return strings.Contains(s, " ")
}

// envNamePattern matches a valid environment variable name.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validEnvName reports whether a name is a valid environment variable identifier.
func validEnvName(name string) bool { return envNamePattern.MatchString(name) }

// isLoopbackHost reports whether a host refers to this machine.
//
// Used to decide whether plain http is acceptable. Anything unrecognised is treated as
// non-loopback, because assuming "this is probably localhost" would downgrade a real
// security warning.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
