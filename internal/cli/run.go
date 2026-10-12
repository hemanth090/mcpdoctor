package cli

import (
	"bufio"
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mcpdoctor/mcpdoctor/internal/config"
	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/protocol"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
	"github.com/mcpdoctor/mcpdoctor/internal/render"
)

// nowFunc returns the current time. It is a variable so that a test can produce a
// deterministic report.
var nowFunc = time.Now

// runResult is everything one command produced.
//
// It is a plain value returned by the run functions rather than accumulated in package
// state, so two concurrent runs cannot interfere and nothing leaks between invocations.
type runResult struct {
	report  diagnose.Report
	sources []render.Source
	servers []render.Server

	// targets are the resolved probe targets, parallel to servers. They are populated
	// before the consent gate and consumed after it, so they must be carried on the
	// result rather than looked up from somewhere global.
	targets []protocol.Target

	redactor *redact.Redactor
	json     bool
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runCheck performs static validation with no execution and no network access.
//
// This is the guarantee that makes `check` safe to run in CI against a repository it
// does not trust: it never reaches the process-spawning or networking code paths.
func runCheck(ctx context.Context, opts Options) runResult {
	r := redact.New()
	result := runResult{redactor: r, json: opts.JSON}

	sources := resolveSources(opts)
	result.report.Checked = len(sources)

	for _, src := range sources {
		res := config.ReadFile(src, config.DefaultLimits())
		rs := render.Source{
			Client:  string(src.Client),
			Path:    r.Redact(src.Path),
			Scope:   string(src.Scope),
			Format:  string(src.Format),
			Exists:  res.Err == nil || res.Err.Kind != config.FileErrNotFound,
			Read:    res.Err == nil,
			Symlink: r.Sanitize(res.SymlinkTarget, 256),
		}

		if res.Err != nil {
			// Severity is a judgement about how broken the situation is, not about how
			// unusual it is. A configuration that cannot be parsed at all is broken: the
			// client will fail to read it, so a CI gate must fail. Classifying it as a
			// warning would let a corrupt config pass a pipeline, which is the one thing
			// this tool exists to prevent.
			severity := diagnose.SeverityError
			switch res.Err.Kind {
			case config.FileErrNotFound:
				// A missing configuration is the normal state of most machines, so it is
				// informational. Failing a build because a user has no Gemini CLI config
				// would make the tool useless.
				severity = diagnose.SeverityInfo
			case config.FileErrUnsupportedFormat:
				// A YAML or TOML client is detected but not parsed in this release. The
				// user's configuration is fine; this tool simply does not read it yet.
				// Calling that an error would fail a CI job over a tool limitation rather
				// than a configuration problem, which is the wrong reason to fail.
				severity = diagnose.SeverityWarning
			}

			rs.Error = r.Sanitize(res.Err.Error(), 400)
			result.report.Add(diagnose.Finding{
				Code:     res.Err.DiagnosticCode(),
				Severity: severity,
				Target:   string(src.Client),
				Location: config.Location(src.Path, res.Pos),
				Message:  fileErrorMessage(res.Err.Kind),
				Detail:   rs.Error,
				Remedy:   fileErrorRemedy(res.Err.Kind),
			})
			result.sources = append(result.sources, rs)
			continue
		}

		if res.SymlinkTarget != "" {
			result.report.Add(diagnose.Finding{
				Code:     diagnose.CodeConfigSymlink,
				Severity: diagnose.SeverityInfo,
				Target:   string(src.Client),
				Location: src.Path,
				Message:  "configuration file is a symbolic link",
				Detail:   "The file resolves to " + r.Sanitize(res.SymlinkTarget, 256) + ". A symlinked config is legitimate, but in a shared directory it is also how a configuration can be swapped without it being obvious.",
				Remedy:   "Confirm the link points where you expect.",
			})
		}

		adapter := adapterFor(src.Client)
		servers, parseFindings := config.ParseServers(adapter, res.Node)
		result.report.Extend(parseFindings)
		result.report.Extend(config.ValidateAll(adapter, servers, r))
		reportDuplicateKeys(res, &result.report)
		rs.ServerCt = len(servers)

		result.sources = append(result.sources, rs)
	}

	result.report.Sort()
	return result
}

// reportDuplicateKeys surfaces JSON keys that appear more than once in one object.
//
// A standard parser silently keeps the last occurrence, so a duplicated key almost
// always means a bad merge, and it changes behaviour invisibly. Records are scoped by
// JSON pointer, so the same field name in two different servers is not a duplicate.
func reportDuplicateKeys(res config.FileResult, report *diagnose.Report) {
	if res.Node == nil {
		return
	}
	for _, pointer := range sortedKeys(res.Node.KeyPositions) {
		positions := res.Node.KeyPositions[pointer]
		if len(positions) < 2 {
			continue
		}
		report.Add(diagnose.Finding{
			Code:     diagnose.CodeConfigDuplicateKey,
			Severity: diagnose.SeverityWarning,
			Target:   string(res.Source.Client),
			Location: config.Location(res.Source.Path, positions[1]),
			Message:  fmt.Sprintf("key %q appears %d times in the same object", lastSegment(pointer), len(positions)),
			Detail:   "A JSON parser keeps the last occurrence and discards the others, so the effective setting may not be the one you intended. This almost always comes from a bad merge or a generated file.",
			Remedy:   "Remove the duplicate key, keeping the one you actually want.",
		})
	}
}

// lastSegment returns the final component of a JSON pointer, for display.
//
// The full pointer is unhelpful in a one-line message; the field name is what the user
// needs to find it.
func lastSegment(pointer string) string {
	if i := strings.LastIndexByte(pointer, '/'); i >= 0 {
		return pointer[i+1:]
	}
	return pointer
}

// sortedKeys returns a map's keys in sorted order, for deterministic iteration.
func sortedKeys(m map[string][]config.Position) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolveSources returns the configuration locations a run should examine.
//
// An explicit --config path is used verbatim and nothing else, because the user asked
// for exactly that file and silently adding others would make the result impossible to
// reason about.
func resolveSources(opts Options) []config.Source {
	if len(opts.ConfigPaths) > 0 {
		out := make([]config.Source, 0, len(opts.ConfigPaths))
		for _, p := range opts.ConfigPaths {
			abs, err := filepath.Abs(p)
			if err != nil {
				abs = p
			}
			client := config.ClientClaudeDesktop
			if detected, ok := detectClient(abs); ok {
				client = detected
			}
			out = append(out, config.Source{
				Client:      client,
				DisplayName: string(client),
				Path:        abs,
				Scope:       scopeFor(abs),
				Format:      detectFormat(abs),
			})
		}
		return out
	}
	return config.DescribePaths(opts.ProjectRoot)
}

// detectFormat infers a configuration file's syntax from its extension.
//
// Without this, pointing --config at a Continue YAML file would report "not valid
// JSON", which is true but useless: the real answer is that MCP Doctor recognises the
// file and does not parse YAML yet. Saying the precise thing is the difference between
// a diagnostic the user can act on and one that sends them to the wrong document.
func detectFormat(path string) config.Format {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return config.FormatYAML
	case ".toml":
		return config.FormatTOML
	default:
		return config.FormatJSON
	}
}

// scopeFor classifies an explicit path as user or project configuration.
func scopeFor(path string) config.Scope {
	if config.IsProjectPath(path) {
		return config.ScopeProject
	}
	return config.ScopeUser
}

// detectClient guesses which client an explicit file belongs to.
//
// It is best-effort by design: an unattributable file is still validated through a
// generic adapter, so an unrecognised client produces diagnostics rather than silence.
func detectClient(path string) (config.Client, bool) {
	clean := filepath.ToSlash(filepath.Clean(path))
	for _, a := range config.Adapters() {
		for _, p := range a.UserPaths() {
			if filepath.Clean(p) == filepath.Clean(path) {
				return a.Client, true
			}
		}
		for _, rel := range a.Projects {
			if strings.HasSuffix(clean, "/"+rel) {
				return a.Client, true
			}
		}
	}
	if strings.HasSuffix(clean, "/.mcp.json") {
		return config.ClientClaudeCode, true
	}
	return config.ClientClaudeDesktop, false
}

// adapterFor returns an adapter for a client, falling back to a permissive generic one.
//
// The fallback matters: a file we could not attribute to a client must still be checked.
// It accepts every transport spelling documented across the supported clients, because
// reporting a correct value as invalid on the strength of not knowing which client wrote
// it would be worse than saying nothing.
func adapterFor(client config.Client) config.Adapter {
	if a, ok := config.AdapterFor(client); ok {
		return a
	}
	return config.Adapter{
		Client:      client,
		DisplayName: string(client),
		Format:      config.FormatJSON,
		ServersKey:  "mcpServers",
		URLKeys:     []string{"url", "httpUrl", "serverUrl"},
		SupportedTypeValues: map[string]config.TransportKind{
			"stdio":           config.TransportStdio,
			"http":            config.TransportHTTP,
			"streamable-http": config.TransportHTTP,
			"streamableHttp":  config.TransportHTTP,
			"sse":             config.TransportSSE,
		},
	}
}

// runProbe performs the consented live protocol diagnostics.
//
// It begins with the static result, so a probe run also reports configuration problems:
// a server that cannot start because its config is wrong should show both, not just the
// symptom.
func runProbe(ctx context.Context, r *run, opts Options) runResult {
	result := runCheck(ctx, Options{
		ConfigPaths: opts.ConfigPaths,
		ProjectRoot: opts.ProjectRoot,
		JSON:        opts.JSON,
		FailOn:      opts.FailOn,
	})
	result.json = opts.JSON

	if !opts.Execute {
		// Refusing here, before any target is even resolved, means the refusal cannot be
		// bypassed by a later code path.
		result.report.Add(diagnose.Finding{
			Code:     diagnose.CodeProbeNotAuthorized,
			Severity: diagnose.SeverityInfo,
			Target:   "probe",
			Message:  "server execution was not authorised, so no server was started",
			Detail:   "Configuration was validated statically. Starting a server runs code named by that file, which MCP Doctor will not do without explicit authorisation.",
			Remedy:   "Re-run with --execute to start the servers, or use `check` for configuration-only validation.",
		})
		result.report.Sort()
		return result
	}

	loadTargets(opts, &result)

	if !confirmExecution(r.env, opts, result.targets, result.redactor) {
		result.report.Add(diagnose.Finding{
			Code:     diagnose.CodeProbeNotAuthorized,
			Severity: diagnose.SeverityInfo,
			Target:   "probe",
			Message:  "execution was declined, so no server was started",
			Detail:   "You were asked to confirm the commands that would run, and declined.",
			Remedy:   "Re-run with --execute and confirm, or pass --yes in a non-interactive context.",
		})
		result.report.Sort()
		return result
	}

	// Probing is sequential by design: it keeps resource use bounded and the report
	// deterministic. A configuration with dozens of servers is not a real workload, and
	// unbounded parallelism would be a resource-exhaustion vector.
	for i := range result.targets {
		probeOne(ctx, r, opts, &result, result.targets[i], &result.servers[i])
	}

	result.report.Sort()
	return result
}

// loadTargets resolves probe targets from configuration without starting anything.
func loadTargets(opts Options, result *runResult) {
	for _, src := range resolveSources(opts) {
		res := config.ReadFile(src, config.DefaultLimits())
		if res.Err != nil {
			continue
		}
		adapter := adapterFor(src.Client)
		servers, _ := config.ParseServers(adapter, res.Node)

		for _, s := range servers {
			if opts.Server != "" && s.Name != opts.Server {
				continue
			}
			target := toTarget(adapter, s)

			result.servers = append(result.servers, render.Server{
				Name:      s.Name,
				Transport: string(target.Kind),
				Probed:    false,
			})
			result.targets = append(result.targets, target)
		}
	}
}

// toTarget converts a validated server entry into a probe target.
func toTarget(a config.Adapter, s config.Server) protocol.Target {
	t := protocol.Target{
		Name:    s.Name,
		Env:     s.Env,
		Headers: s.Headers,
	}

	switch s.Transport {
	case config.TransportHTTP, config.TransportSSE:
		t.Kind = protocol.KindHTTP
		t.URL = s.URL
	case config.TransportStdio:
		t.Kind = protocol.KindStdio
		t.Command = s.Command
		t.Args = s.Args
		t.Cwd = s.Cwd
	}
	return t
}

// probeOne runs a single server probe and records the summary and findings.
func probeOne(ctx context.Context, r *run, opts Options, result *runResult, target protocol.Target, entry *render.Server) {
	// Probing a remote endpoint is a separate consent decision from running a local
	// process: the operator may have intended "start my local servers" without realising
	// that one of them points at a public URL. --allow-remote is the explicit override.
	allowRemote := true
	if target.Kind == protocol.KindHTTP && isRemote(target.URL) {
		allowRemote = opts.AllowRemote || confirmRemote(r.env, target)
		if !allowRemote {
			result.report.Add(diagnose.Finding{
				Code:     diagnose.CodeRemoteProbeBlocked,
				Severity: diagnose.SeverityInfo,
				Target:   entry.Name,
				Message:  "skipped a remote endpoint that was not authorised for probing",
				Detail:   "The server " + entry.Name + " points at " + result.redactor.Redact(target.URL) + ", which is not on this machine. Contacting it makes a network request, so it requires --allow-remote.",
				Remedy:   "Pass --allow-remote if you intend to contact this endpoint.",
			})
			return
		}
	}

	res, err := protocol.Probe(ctx, target, protocol.Options{
		Execute:     true,
		AllowRemote: allowRemote,
		Timeout:     probeTimeout(r),
	}, result.redactor)
	if err != nil {
		result.report.Add(diagnose.Finding{
			Code:     diagnose.CodeProbeNotAuthorized,
			Severity: diagnose.SeverityError,
			Target:   entry.Name,
			Message:  "probe could not be performed",
			Detail:   result.redactor.Sanitize(err.Error(), 200),
			Remedy:   "Check the server configuration.",
		})
		return
	}

	entry.Probed = true
	entry.NegotiatedVersion = res.NegotiatedVersion
	entry.ServerName = res.ServerName
	entry.ServerVersion = res.ServerVersion
	entry.Capabilities = res.Capabilities
	entry.ToolCount = res.ToolCount
	entry.DurationMS = res.Duration.Milliseconds()

	result.report.Extend(res.Findings)
}

// probeTimeout returns the configured per-probe timeout.
func probeTimeout(r *run) time.Duration {
	if r.timeout > 0 {
		return r.timeout
	}
	return protocol.DefaultTimeout
}

// isRemote reports whether a URL points off this machine.
func isRemote(rawURL string) bool {
	lower := strings.ToLower(rawURL)
	for _, local := range []string{
		"http://127.0.0.1", "https://127.0.0.1",
		"http://localhost", "https://localhost",
		"http://[::1]", "https://[::1]",
	} {
		if strings.HasPrefix(lower, local) {
			return false
		}
	}
	return true
}

// confirmExecution asks the operator to approve the commands that would run.
//
// The prompt is the control that makes probing safe to offer at all. It prints the exact
// command, waits for an explicit yes, and refuses to guess when there is no terminal to
// ask. A default of "no" means pressing Enter does the safe thing.
func confirmExecution(env Env, opts Options, targets []protocol.Target, r *redact.Redactor) bool {
	if len(targets) == 0 {
		return true
	}
	if opts.Yes {
		// --yes is the explicit non-interactive override, and it only has meaning
		// alongside --execute, which the caller already required.
		return true
	}

	if !env.IsTerminal {
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, t.Name)
		}
		sort.Strings(names)
		fmt.Fprintf(env.Stderr,
			"mcp-doctor: refusing to start %d server(s) without confirmation.\n"+
				"This is a non-interactive context, so there is nobody to ask.\n"+
				"Pass --execute --yes if you intend to run: %s\n",
			len(names), strings.Join(names, ", "))
		return false
	}

	fmt.Fprintf(env.Stderr, "\nThe following %d server(s) will be started. Each runs code from your configuration:\n\n", len(targets))
	for _, t := range targets {
		if t.Kind == protocol.KindStdio {
			fmt.Fprintf(env.Stderr, "  %s: %s %s\n", t.Name, t.Command, strings.Join(r.RedactArgs(t.Args), " "))
		} else {
			fmt.Fprintf(env.Stderr, "  %s: connect to %s\n", t.Name, r.Redact(t.URL))
		}
	}
	fmt.Fprint(env.Stderr, "\nStart them? [y/N] ")

	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// confirmRemote asks before making a network request to a non-loopback endpoint.
//
// It defaults to refusing, and it never prompts without a terminal, because a tool that
// silently starts sending traffic to the internet the moment it is pointed at an HTTPS
// URL is exactly the failure mode the consent model exists to prevent.
func confirmRemote(env Env, target protocol.Target) bool {
	if !env.IsTerminal {
		return false
	}
	fmt.Fprintf(env.Stderr, "\n%s points at %s, which is not on this machine.\nContacting it makes a network request.\n\nContact it? [y/N] ",
		target.Name, redactURL(target.URL))

	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// redactURL returns the URL with any embedded credentials removed, so the prompt cannot
// print a password the user did not intend to show on screen.
func redactURL(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = neturl.UserPassword("REDACTED", "REDACTED")
	return u.String()
}

// fileErrorMessage renders a short message for a file error kind.
func fileErrorMessage(kind string) string {
	switch kind {
	case config.FileErrNotFound:
		return "no configuration file at this location"
	case config.FileErrTooLarge:
		return "configuration file is too large to read safely"
	case config.FileErrUnsupportedFormat:
		return "configuration format is not supported in this release"
	case config.FileErrInvalid:
		return "configuration file is not valid JSON"
	default:
		return "configuration file could not be read"
	}
}

// fileErrorRemedy renders the recommended action for a file error kind.
func fileErrorRemedy(kind string) string {
	switch kind {
	case config.FileErrNotFound:
		return "This is normal if you have not configured this client yet. Create the file only if you intend to use this client."
	case config.FileErrTooLarge:
		return "MCP Doctor refuses to read configuration files above the size limit, because loading an arbitrarily large file is a denial-of-service risk. Check that the path points at the configuration you meant."
	case config.FileErrUnsupportedFormat:
		return "This release validates JSON configurations. YAML and TOML clients are detected and reported, not parsed."
	case config.FileErrInvalid:
		return "Fix the JSON syntax at the reported line. The location above points at the problem."
	default:
		return "Check that the file exists and that you have permission to read it."
	}
}
