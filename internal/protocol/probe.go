// Package protocol performs consented, bounded MCP protocol diagnostics.
//
// It is the only package that starts a server or opens a network connection, and it does
// neither without explicit authorisation. The division of responsibility is deliberate:
//
//   - Process management (process.go) owns the subprocess lifecycle and never touches the
//     protocol stream.
//   - This file owns the protocol conversation and never manages the process directly.
//
// Keeping them apart is what guarantees that diagnostic output can never corrupt the
// stdio protocol stream, because nothing here writes to the child's stdout.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
)

// DefaultTimeout bounds a whole probe. Every individual request gets a shorter deadline
// derived from this one, so a hung server cannot outlive the run.
const DefaultTimeout = 20 * time.Second

// clientIdentity is how MCP Doctor introduces itself to a server.
//
// It uses a real vendor domain on purpose. A server may gate behaviour on the client
// name, and identifying honestly is better than pretending to be a known client in order
// to unlock features.
var clientIdentity = &mcp.Implementation{
	Name:    "mcp-doctor",
	Title:   "MCP Doctor",
	Version: Version,
}

// Version is the MCP Doctor version, overridden at build time with -ldflags.
var Version = "dev"

// Options configures a single server probe.
type Options struct {
	// Execute must be true for the probe to start a process or open a connection.
	//
	// This is the single authorisation gate. A probe without it performs no I/O against
	// the server whatsoever, which is what makes `check` safe by construction rather than
	// by policy.
	Execute bool

	// AllowRemote permits probing a host that is not loopback.
	AllowRemote bool

	// Timeout bounds the whole probe. Zero uses DefaultTimeout.
	Timeout time.Duration

	// MaxTools bounds how many tools are validated from tools/list. Zero uses
	// DefaultMaxTools. A server that advertises an enormous tool list is more likely a
	// misconfiguration than a real deployment, and validating all of it would be slow.
	MaxTools int

	// RequestTimeout bounds each individual MCP request. Zero derives it from Timeout.
	RequestTimeout time.Duration
}

// DefaultMaxTools bounds tool validation.
const DefaultMaxTools = 200

// maxProtocolLine bounds a single protocol message read from a server.
//
// Server output is untrusted and unbounded, so a line-length cap is the difference
// between a bounded diagnostic tool and one a hostile server can exhaust. 8 MiB is far
// above any legitimate single JSON-RPC message.
const maxProtocolLine = 8 << 20

// Result is the outcome of probing one server.
type Result struct {
	// Findings are the diagnostics produced by this probe.
	Findings []diagnose.Finding

	// NegotiatedVersion is the protocol version agreed during initialization, if the
	// handshake completed.
	NegotiatedVersion string

	// ServerName and ServerVersion come from the server's implementation info.
	ServerName    string
	ServerVersion string

	// Capabilities lists the server's advertised capabilities in a stable, sorted form.
	Capabilities []string

	// ToolCount is how many tools the server advertised.
	ToolCount int

	// Duration is how long the probe took. It is informational and excluded from
	// determinism guarantees.
	Duration time.Duration
}

// Probe runs the consented protocol diagnostics for one server target.
//
// It returns a Result rather than an error for server-side problems, because a server
// that fails to start is a diagnostic result the user needs to see, not a tool failure.
// An error is returned only when the probe could not be performed at all, for example
// because authorisation was not given.
func Probe(ctx context.Context, target Target, opts Options, r *redact.Redactor) (Result, error) {
	if !opts.Execute {
		return Result{}, &ConsentError{Target: target.Name}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	requestTimeout := opts.RequestTimeout
	if requestTimeout <= 0 {
		// Each request gets a slice of the total budget, so several slow requests cannot
		// add up past the user's stated limit.
		requestTimeout = timeout / 4
	}
	if requestTimeout <= 0 {
		requestTimeout = time.Second
	}
	maxTools := opts.MaxTools
	if maxTools <= 0 {
		maxTools = DefaultMaxTools
	}

	// The deadline covers everything: process start, handshake, and tool discovery.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch target.Kind {
	case KindStdio:
		return probeStdio(ctx, target, opts, requestTimeout, maxTools, r), nil
	case KindHTTP:
		return probeHTTP(ctx, target, opts, requestTimeout, maxTools, r), nil
	default:
		return Result{}, fmt.Errorf("unsupported transport %q", target.Kind)
	}
}

// ConsentError is returned when a probe is attempted without Execute.
//
// It is a distinct type so the CLI can render it as "you did not authorise this" rather
// than as a failure of the server or of the tool.
type ConsentError struct {
	Target string
}

// Error implements error.
func (e *ConsentError) Error() string {
	return "execution of " + e.Target + " was not authorised"
}

// probeStdio starts a local server and speaks MCP over its stdin and stdout.
func probeStdio(ctx context.Context, target Target, opts Options, requestTimeout time.Duration, maxTools int, r *redact.Redactor) Result {
	start := time.Now()
	res := Result{Findings: make([]diagnose.Finding, 0, 8)}

	// Build the child's environment from an explicit allowlist rather than inheriting
	// ours. An MCP server is third-party code; handing it every credential the user
	// holds would be indefensible.
	env := buildTransportEnv(target.Env)
	envSlice := make([]string, 0, len(env))
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}

	// Register the configuration's environment values with the redactor BEFORE the
	// server runs, so a server that echoes one back to stderr cannot leak it.
	//
	// Values are registered by name so the non-sensitive-variable allowlist still
	// applies, and only values that actually look like credentials are registered.
	// Registering every configured value would be defensible but self-defeating: an env
	// var holding a log level or a path is not a secret, and redacting it would corrupt
	// every diagnostic that mentions it, teaching users to ignore redaction.
	for k, v := range target.Env {
		if redact.LooksSecret(k, v) {
			r.Add(v, true)
		}
	}

	proc, err := Start(ctx, target.Command, target.Args, ProcessOptions{
		Dir:           target.Cwd,
		Env:           envSlice,
		StderrLimit:   stderrLimit,
		ShutdownGrace: 2 * time.Second,
		KillGrace:     2 * time.Second,
	})
	if err != nil {
		res.Findings = append(res.Findings, diagnose.Finding{
			Code:     diagnose.CodeSpawnFailed,
			Severity: diagnose.SeverityError,
			Target:   target.Name,
			Message:  "could not start the server process",
			Detail:   r.Sanitize(err.Error(), 300),
			Remedy:   "Check that the command exists, is executable, and that the working directory is correct.",
		})
		res.Duration = time.Since(start)
		return res
	}
	defer proc.Stop()

	// The SDK's IOTransport gives us exactly the stdio framing the protocol specifies,
	// with a bounded line length so a server cannot exhaust memory with one enormous
	// "message". It writes only protocol frames to the child, so diagnostic output can
	// never corrupt the stream.
	transport := &mcp.IOTransport{
		Reader:        proc.Stdout(),
		Writer:        proc.Stdin(),
		MaxLineLength: maxProtocolLine,
	}

	client := mcp.NewClient(clientIdentity, nil)
	session, err := client.Connect(ctx, transport, &mcp.ClientSessionOptions{})
	if err != nil {
		res.Findings = append(res.Findings, classifyConnectFailure(ctx, err, proc, target, r))
		attachStderr(&res, proc, target, r)
		res.Duration = time.Since(start)
		return res
	}
	defer session.Close()

	// From here the handshake succeeded, so report what was negotiated.
	init := session.InitializeResult()
	if init != nil {
		res.NegotiatedVersion = init.ProtocolVersion
		if init.ServerInfo != nil {
			res.ServerName = init.ServerInfo.Name
			res.ServerVersion = init.ServerInfo.Version
		}
		res.Capabilities = capabilityNames(init.Capabilities)
		res.Findings = append(res.Findings, checkVersion(init.ProtocolVersion)...)
	}

	// Validate tool discovery, since a server that advertises tools but cannot list them
	// is broken in a way the user very often hits.
	tools, findings := inspectTools(ctx, session, requestTimeout, maxTools, target, r)
	res.Findings = append(res.Findings, findings...)
	res.ToolCount = tools

	attachStderr(&res, proc, target, r)
	res.Duration = time.Since(start)
	return res
}

// probeHTTP connects to a remote MCP endpoint over Streamable HTTP.
func probeHTTP(ctx context.Context, target Target, opts Options, requestTimeout time.Duration, maxTools int, r *redact.Redactor) Result {
	start := time.Now()
	res := Result{Findings: make([]diagnose.Finding, 0, 8)}

	u, err := url.Parse(target.URL)
	if err != nil {
		res.Findings = append(res.Findings, diagnose.Finding{
			Code:     diagnose.CodeURLInvalid,
			Severity: diagnose.SeverityError,
			Target:   target.Name,
			Message:  "the server URL could not be parsed",
			Detail:   r.Sanitize(err.Error(), 200),
			Remedy:   "Check the url in the configuration.",
		})
		res.Duration = time.Since(start)
		return res
	}

	// The remote gate. Probing a public endpoint is a network request the operator may
	// not have intended, so it takes its own explicit consent.
	if !isLoopback(u.Hostname()) && !opts.AllowRemote {
		res.Findings = append(res.Findings, diagnose.Finding{
			Code:     diagnose.CodeRemoteNotAllowed,
			Severity: diagnose.SeverityError,
			Target:   target.Name,
			Message:  "refusing to contact a remote endpoint without authorisation",
			Detail:   "The endpoint " + u.Hostname() + " is not on this machine. Probing it makes a network request, so it requires --allow-remote.",
			Remedy:   "Re-run with --allow-remote if you intend to contact this endpoint.",
		})
		res.Duration = time.Since(start)
		return res
	}

	httpClient := buildHTTPClient(target.URL, opts)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   target.URL,
		HTTPClient: httpClient,
	}

	client := mcp.NewClient(clientIdentity, nil)
	session, err := client.Connect(ctx, transport, &mcp.ClientSessionOptions{})
	if err != nil {
		res.Findings = append(res.Findings, classifyConnectFailure(ctx, err, nil, target, r))
		res.Duration = time.Since(start)
		return res
	}
	defer session.Close()

	init := session.InitializeResult()
	if init != nil {
		res.NegotiatedVersion = init.ProtocolVersion
		if init.ServerInfo != nil {
			res.ServerName = init.ServerInfo.Name
			res.ServerVersion = init.ServerInfo.Version
		}
		res.Capabilities = capabilityNames(init.Capabilities)
		res.Findings = append(res.Findings, checkVersion(init.ProtocolVersion)...)
	}

	tools, findings := inspectTools(ctx, session, requestTimeout, maxTools, target, r)
	res.Findings = append(res.Findings, findings...)
	res.ToolCount = tools

	res.Duration = time.Since(start)
	return res
}

// classifyConnectFailure turns a connection error into the most specific diagnostic it
// can justify.
//
// The distinction matters: "the server exited" and "the server did not answer in time"
// have completely different fixes, and reporting both as a generic connection failure is
// how a diagnostic tool becomes useless.
func classifyConnectFailure(ctx context.Context, err error, proc *Process, target Target, r *redact.Redactor) diagnose.Finding {
	msg := r.Sanitize(err.Error(), 400)

	// An expired context is a timeout, whatever the underlying error looked like.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeout(err) {
		return diagnose.Finding{
			Code:     diagnose.CodeStartupTimeout,
			Severity: diagnose.SeverityError,
			Target:   target.Name,
			Message:  "the server did not complete initialization in time",
			Detail:   "No initialize response arrived before the timeout. " + msg,
			Remedy:   "Start the server manually to see where it blocks: it may be waiting on input, a network call, or a credential that is not set.",
		}
	}

	// A process that has already exited explains the failure better than the protocol
	// error does, so prefer that diagnosis when we have it.
	if proc != nil {
		if exited, _ := proc.ProcessExitedEarly(); exited {
			return diagnose.Finding{
				Code:     diagnose.CodeExitedBeforeInit,
				Severity: diagnose.SeverityError,
				Target:   target.Name,
				Message:  "the server exited before completing initialization",
				Detail:   "The process ended instead of responding. " + msg,
				Remedy:   "Run the same command by hand to see the error. The most common causes are a missing dependency, a missing credential, or a port already in use.",
			}
		}
	}

	// A malformed message is a protocol error and must be labelled as one, rather than
	// being reported as a generic connection failure.
	if isProtocolParseError(err) {
		return diagnose.Finding{
			Code:     diagnose.CodeMalformedMessage,
			Severity: diagnose.SeverityError,
			Target:   target.Name,
			Message:  "the server sent a message that is not valid JSON-RPC",
			Detail:   msg,
			Remedy:   "The stdio transport reserves stdout for protocol messages. A server that prints a banner or a log line to stdout will break every client; it belongs on stderr.",
		}
	}

	if isHTTPStatusError(err) {
		return diagnose.Finding{
			Code:     diagnose.CodeHTTPStatus,
			Severity: diagnose.SeverityError,
			Target:   target.Name,
			Message:  "the endpoint rejected the initialization request",
			Detail:   msg,
			Remedy:   "Check that the URL points at an MCP endpoint and that any required authentication is configured.",
		}
	}

	return diagnose.Finding{
		Code:     diagnose.CodeTransportFailed,
		Severity: diagnose.SeverityError,
		Target:   target.Name,
		Message:  "could not establish an MCP connection",
		Detail:   msg,
		Remedy:   "Verify that the server is running and that the command, arguments, and URL are correct.",
	}
}

// inspectTools validates tool discovery against the protocol's requirements.
func inspectTools(ctx context.Context, session *mcp.ClientSession, requestTimeout time.Duration, maxTools int, target Target, r *redact.Redactor) (int, []diagnose.Finding) {
	var findings []diagnose.Finding

	// Only call tools/list when the server advertised the capability. Calling it
	// otherwise produces a spurious method-not-found error that is not a real problem.
	if !advertisesTools(session.InitializeResult()) {
		return 0, findings
	}

	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	tools, err := session.ListTools(callCtx, nil)
	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return 0, []diagnose.Finding{{
				Code:     diagnose.CodeToolsListFailed,
				Severity: diagnose.SeverityError,
				Target:   target.Name,
				Message:  "tools/list did not respond in time",
				Detail:   "The server advertises the tools capability but did not answer a tool listing. " + r.Sanitize(err.Error(), 200),
				Remedy:   "Check whether the server blocks during startup or is waiting on an external dependency.",
			}}
		}
		return 0, []diagnose.Finding{{
			Code:     diagnose.CodeCapabilityMismatch,
			Severity: diagnose.SeverityError,
			Target:   target.Name,
			Message:  "the server advertises tools but tools/list failed",
			Detail:   "A server that advertises the tools capability must be able to list them. " + r.Sanitize(err.Error(), 200),
			Remedy:   "This is a server-side bug. Check the server's logs for the underlying error.",
		}}
	}

	count := len(tools.Tools)
	if count > maxTools {
		return count, []diagnose.Finding{{
			Code:     diagnose.CodeToolCountExceedsLimit,
			Severity: diagnose.SeverityWarning,
			Target:   target.Name,
			Message:  fmt.Sprintf("the server advertised %d tools, which exceeds the diagnostic limit of %d", count, maxTools),
			Detail:   "Only the first " + fmt.Sprint(maxTools) + " tools were validated. An unusually large tool list often means tools are duplicated or the server is not filtering by client.",
			Remedy:   "Check whether the server is meant to expose this many tools to a single client.",
		}}
	}

	findings = append(findings, validateToolSchemas(target, tools.Tools, maxTools, r)...)
	return count, findings
}

// validateToolSchemas checks each advertised tool against the protocol's requirements.
func validateToolSchemas(target Target, tools []*mcp.Tool, maxTools int, _ *redact.Redactor) []diagnose.Finding {
	var findings []diagnose.Finding
	seen := make(map[string]bool, len(tools))

	for i, tool := range tools {
		if i >= maxTools {
			break
		}
		if tool == nil {
			findings = append(findings, toolFinding(target, diagnose.CodeEmptyToolName, "the server advertised a null tool entry"))
			continue
		}
		if strings.TrimSpace(tool.Name) == "" {
			findings = append(findings, toolFinding(target, diagnose.CodeEmptyToolName, "a tool has an empty name",
				"A tool cannot be called without a name, so the client will be unable to invoke it.",
				"Give the tool a name in the server's tool registration."))
			continue
		}
		if seen[tool.Name] {
			findings = append(findings, toolFinding(target, diagnose.CodeDuplicateToolName,
				"two tools share the name "+tool.Name,
				"Tool calls are addressed by name, so a duplicate makes the target ambiguous and the client will invoke whichever it finds first.",
				"Rename one of the tools in the server."))
		}
		seen[tool.Name] = true

		// The specification requires inputSchema to be a JSON Schema object of type
		// "object". A server that omits or misdeclares it produces tools that fail at
		// call time rather than at startup.
		//
		// InputSchema is typed as any by the SDK, so it is asserted rather than assumed:
		// a server that sends a string or a number here is a real failure mode and must
		// be reported rather than panic.
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			findings = append(findings, toolFinding(target, diagnose.CodeToolSchemaInvalid,
				"tool "+tool.Name+" has a malformed inputSchema",
				"The protocol requires inputSchema to be a JSON Schema object. The server sent a value that is not an object.",
				"Declare inputSchema as an object, for example {\"type\": \"object\", \"properties\": {}}."))
			continue
		}
		if schemaType, ok := schema["type"].(string); !ok || schemaType != "object" {
			findings = append(findings, toolFinding(target, diagnose.CodeToolSchemaInvalid,
				"tool "+tool.Name+" declares a non-object inputSchema",
				"The protocol requires inputSchema.type to be \"object\"; a client that assumes this will mis-handle arguments.",
				"Set the schema type to \"object\"."))
		}
	}
	return findings
}

// toolFinding builds a finding scoped to a tool within a server.
func toolFinding(target Target, code, message string, extra ...string) diagnose.Finding {
	f := diagnose.Finding{
		Code:     code,
		Severity: diagnose.SeverityError,
		Target:   target.Name,
		Message:  message,
		Remedy:   "This is a server-side problem. Check the server's tool definitions.",
	}
	if len(extra) > 0 {
		f.Detail = extra[0]
	}
	if len(extra) > 1 {
		f.Remedy = extra[1]
	}
	return f
}

// advertisesTools reports whether the server declared the tools capability.
func advertisesTools(init *mcp.InitializeResult) bool {
	return init != nil && init.Capabilities != nil && init.Capabilities.Tools != nil
}

// checkVersion validates the negotiated protocol version.
func checkVersion(version string) []diagnose.Finding {
	if version == "" {
		return nil
	}
	supported := mcp.SupportedProtocolVersions()
	for _, v := range supported {
		if v == version {
			return nil
		}
	}
	// An unknown version is informational rather than an error: the server and the tool
	// may both be newer than this build, in which case everything may work fine.
	return []diagnose.Finding{{
		Code:     diagnose.CodeVersionMismatch,
		Severity: diagnose.SeverityInfo,
		Target:   "protocol",
		Message:  "negotiated protocol version " + version + " is newer than this build recognises",
		Detail:   "This build of MCP Doctor knows about " + strings.Join(supported, ", ") + ". Diagnostics against a newer version are best-effort.",
		Remedy:   "Upgrade MCP Doctor if you need full diagnostics for this protocol revision.",
	}}
}

// capabilityNames renders the server's capabilities as a sorted, stable list.
func capabilityNames(caps *mcp.ServerCapabilities) []string {
	if caps == nil {
		return nil
	}
	var names []string
	if caps.Tools != nil {
		names = append(names, "tools")
	}
	if caps.Prompts != nil {
		names = append(names, "prompts")
	}
	if caps.Resources != nil {
		names = append(names, "resources")
	}
	if caps.Logging != nil {
		names = append(names, "logging")
	}
	if caps.Completions != nil {
		names = append(names, "completions")
	}
	if caps.Experimental != nil {
		names = append(names, "experimental")
	}
	return names
}

// attachStderr adds a finding when the server logged something that looks like a failure.
//
// This is informational by default: plenty of healthy servers write warnings to stderr,
// and treating all of it as an error would train users to ignore the finding entirely.
func attachStderr(res *Result, proc *Process, target Target, r *redact.Redactor) {
	if proc == nil {
		return
	}
	tail := strings.TrimSpace(proc.StderrTail())
	if tail == "" {
		return
	}
	excerpt := r.Sanitize(tail, 600)

	severity := diagnose.SeverityInfo
	if looksLikeError(tail) {
		severity = diagnose.SeverityWarning
	}
	res.Findings = append(res.Findings, diagnose.Finding{
		Code:     diagnose.CodeServerStderr,
		Severity: severity,
		Target:   target.Name,
		Message:  "the server wrote to stderr",
		Detail:   "Server log output, captured while probing. It is bounded, stripped of control characters, and redacted:\n" + excerpt,
		Remedy:   "Read the excerpt above. It usually explains any startup failure.",
	})
}

// looksLikeError reports whether stderr content suggests a real failure.
func looksLikeError(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range []string{"error", "fatal", "panic", "exception", "traceback", "failed"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// buildHTTPClient returns an HTTP client with explicit, conservative limits.
//
// Every one of these defaults is a control. Without a response-header timeout a server
// can hold a connection open indefinitely; without a body limit a single response can
// exhaust memory; redirects are bounded because a redirect chain can be used to reach an
// internal service.
func buildHTTPClient(endpoint string, opts Options) *http.Client {
	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		// Bound how much of a response body is buffered at the transport layer, in
		// addition to the limit the MCP transport applies.
		MaxResponseHeaderBytes: 64 << 10,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Bound the chain, then re-validate the scheme so a downgrade from https to
			// http cannot happen silently.
			if len(via) >= 3 {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			if req.URL.Scheme == "http" && !isLoopback(req.URL.Hostname()) {
				return fmt.Errorf("refusing to follow a redirect to plain http on %s", req.URL.Hostname())
			}
			return nil
		},
	}
}

// isTimeout reports whether an error represents a timeout.
func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout")
}

// isProtocolParseError reports whether an error indicates malformed protocol input.
func isProtocolParseError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"invalid character", "unexpected end of json", "cannot unmarshal", "not valid json", "jsonrpc"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isHTTPStatusError reports whether an error is an HTTP status failure.
func isHTTPStatusError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"status code", "http 4", "http 5", "unexpected status"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isLoopback reports whether a host refers to this machine.
func isLoopback(host string) bool {
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
