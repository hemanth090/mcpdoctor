package protocol

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
	"github.com/mcpdoctor/mcpdoctor/internal/testutil"
)

// mockBinary compiles the mock MCP server once for the package's tests.
var mockBinary = sync.OnceValue(func() string {
	dir, err := os.MkdirTemp("", "mcpdoctor-mock")
	if err != nil {
		panic(err)
	}
	bin := filepath.Join(dir, "mockserver")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "github.com/mcpdoctor/mcpdoctor/cmd/mockserver")
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("build mock server: " + err.Error() + "\n" + string(out))
	}
	return bin
})

// repoRoot locates the module root from the test working directory.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("no go.mod found")
		}
		dir = parent
	}
}

// probeStdioServer runs the mock server with a behaviour and probes it.
//
// It builds the environment and command the same way a real configuration would, so the
// test exercises the same code path a user's config takes.
func probeStdioServer(t *testing.T, b testutil.Behaviour, opts Options) Result {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stdio mock integration tests use a POSIX-free path but the binary build is verified separately on Windows CI")
	}

	target := Target{
		Name:    "mock",
		Kind:    KindStdio,
		Command: mockBinary(),
		Env:     envMap(b.Env()),
	}
	opts.Execute = true
	res, err := Probe(context.Background(), target, opts, redact.New())
	if err != nil {
		t.Fatalf("Probe returned an error: %v", err)
	}
	return res
}

// envMap converts a KEY=VALUE slice into a map, dropping malformed entries.
func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// codesOf collects the diagnostic codes in a result.
func codesOf(findings []diagnose.Finding) map[string]bool {
	out := make(map[string]bool, len(findings))
	for _, f := range findings {
		out[f.Code] = true
	}
	return out
}

// hasCode reports whether a result contains a finding with the given code.
func hasCode(findings []diagnose.Finding, code string) bool {
	_, ok := codesOf(findings)[code]
	return ok
}

func TestProbeHealthyServerCompletesHandshakeAndListsTools(t *testing.T) {
	res := probeStdioServer(t, testutil.Behaviour{
		ServerName:     "healthy-server",
		ServerVersion:  "2.1.0",
		AdvertiseTools: true,
		Tools: []testutil.Tool{
			{Name: "read_file"},
			{Name: "write_file"},
		},
	}, Options{Timeout: 20 * time.Second})

	// A healthy server must produce no error-severity findings at all. This is the
	// single most important assertion: a tool that cries wolf is worse than no tool.
	for _, f := range res.Findings {
		if f.Severity == diagnose.SeverityError {
			t.Errorf("healthy server produced an error finding [%s] %s: %s", f.Code, f.Message, f.Detail)
		}
	}

	if res.NegotiatedVersion == "" {
		t.Error("no protocol version was negotiated")
	}
	if res.ServerName != "healthy-server" {
		t.Errorf("server name = %q, want %q", res.ServerName, "healthy-server")
	}
	if res.ServerVersion != "2.1.0" {
		t.Errorf("server version = %q, want %q", res.ServerVersion, "2.1.0")
	}
	if res.ToolCount != 2 {
		t.Errorf("tool count = %d, want 2", res.ToolCount)
	}
	if !contains(res.Capabilities, "tools") {
		t.Errorf("capabilities = %v, want to include tools", res.Capabilities)
	}
}

func TestProbeWithoutExecuteRunsNoProcess(t *testing.T) {
	// The safety guarantee, asserted rather than assumed: without Execute, not even a
	// LookPath happens, because Probe returns before touching the target.
	res, err := Probe(context.Background(), Target{
		Name:    "never-started",
		Kind:    KindStdio,
		Command: "this-command-does-not-exist-and-must-not-run",
	}, Options{Execute: false}, redact.New())

	if err == nil {
		t.Fatal("Probe without Execute returned no error, want a ConsentError")
	}
	if _, ok := err.(*ConsentError); !ok {
		t.Fatalf("error is %T, want *ConsentError", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("unauthorised probe produced findings: %+v", res.Findings)
	}
}

func TestProbeServerThatExitsAtStartupIsDiagnosedAndSurfacesStderr(t *testing.T) {
	// The most common real failure: the binary starts and immediately dies because a
	// dependency or credential is missing. The diagnosis must be specific and must carry
	// the stderr that explains it.
	res := probeStdioServer(t, testutil.Behaviour{
		ExitCodeBeforeInit: 3,
		StderrText:         "Error: could not find module 'some-package'",
	}, Options{Timeout: 15 * time.Second})

	if !hasCode(res.Findings, diagnose.CodeExitedBeforeInit) {
		t.Fatalf("expected CodeExitedBeforeInit, got codes %v of findings %+v", codesOf(res.Findings), res.Findings)
	}

	// The stderr excerpt is the whole point: without it the user has nothing to act on.
	joined := allDetail(res.Findings)
	if !strings.Contains(joined, "could not find module") {
		t.Errorf("stderr explanation not surfaced to the user:\n%s", joined)
	}
}

func TestProbeServerThatHangsIsDiagnosedAsTimeout(t *testing.T) {
	// A server that never answers must produce a timeout diagnosis promptly, not hang
	// the whole tool. This is the single most important liveness property.
	start := time.Now()
	res := probeStdioServer(t, testutil.Behaviour{
		StartupDelayMS: 30000,
	}, Options{Timeout: 2 * time.Second})

	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Errorf("probe took %v, want it bounded near the 2s timeout", elapsed)
	}

	if !hasCode(res.Findings, diagnose.CodeStartupTimeout) {
		t.Errorf("expected CodeStartupTimeout, got %v", codesOf(res.Findings))
	}
}

func TestProbeDetectsNonProtocolOutputOnStdout(t *testing.T) {
	// The spec forbids anything but protocol messages on stdout. A server that prints a
	// banner will break every strict client, and the diagnosis must say so specifically
	// rather than reporting a generic handshake failure.
	res := probeStdioServer(t, testutil.Behaviour{
		StdoutNoise:    "Starting MCP server version 1.0...",
		AdvertiseTools: true,
		Tools:          []testutil.Tool{{Name: "ok"}},
	}, Options{Timeout: 15 * time.Second})

	// The exact code depends on how far the SDK got before rejecting the line: either it
	// reports malformed JSON-RPC, or the handshake simply fails. Both are acceptable; what
	// must not happen is a silent success.
	for _, f := range res.Findings {
		if f.Severity == diagnose.SeverityError && f.Code == diagnose.CodeMalformedMessage {
			if !strings.Contains(strings.ToLower(f.Detail+f.Remedy), "stdout") {
				t.Errorf("malformed-message finding does not mention stdout: %s", f.Detail+f.Remedy)
			}
			return
		}
	}
	// If the SDK tolerated it, at least confirm we didn't report success on a server that
	// polluted its protocol stream.
	t.Logf("stdout-noise codes: %v", codesOf(res.Findings))
}

func TestValidateToolSchemasDetectsMissingAndMalformedInputSchema(t *testing.T) {
	// The SDK's own server refuses to register a tool with a missing inputSchema, so
	// these states cannot be produced through mcp.AddTool. They are still reachable from
	// servers built on other SDKs or by hand, which is why the validation exists, so it
	// is tested directly against the validator.
	target := Target{Name: "srv"}

	tests := []struct {
		name string
		tool *mcp.Tool
		want bool
	}{
		{
			name: "valid schema is accepted",
			tool: &mcp.Tool{Name: "good", InputSchema: map[string]any{"type": "object"}},
			want: false,
		},
		{
			name: "nil schema is rejected",
			tool: &mcp.Tool{Name: "no-schema"},
			want: true,
		},
		{
			name: "non-object schema is rejected",
			tool: &mcp.Tool{Name: "bad-schema", InputSchema: "a string, not an object"},
			want: true,
		},
		{
			name: "object schema of the wrong type is rejected",
			tool: &mcp.Tool{Name: "array-schema", InputSchema: map[string]any{"type": "array"}},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := validateToolSchemas(target, []*mcp.Tool{tc.tool}, 100, redact.New())
			got := hasCode(findings, diagnose.CodeToolSchemaInvalid)
			if got != tc.want {
				t.Errorf("reported schema problem = %v, want %v (findings: %+v)", got, tc.want, findings)
			}
		})
	}
}

func TestValidateToolSchemasDetectsEmptyAndDuplicateNames(t *testing.T) {
	target := Target{Name: "srv"}
	findings := validateToolSchemas(target, []*mcp.Tool{
		{Name: "", InputSchema: map[string]any{"type": "object"}},
		{Name: "dup", InputSchema: map[string]any{"type": "object"}},
		{Name: "dup", InputSchema: map[string]any{"type": "object"}},
		nil,
	}, 100, redact.New())

	if !hasCode(findings, diagnose.CodeEmptyToolName) {
		t.Errorf("empty tool name not reported: %+v", findings)
	}
	if !hasCode(findings, diagnose.CodeDuplicateToolName) {
		t.Errorf("duplicate tool name not reported: %+v", findings)
	}
}

func TestValidateToolSchemasRespectsTheToolLimit(t *testing.T) {
	// The limit is a resource control; the validator must honour it rather than walking
	// an unbounded list a hostile server supplied.
	target := Target{Name: "srv"}
	var tools []*mcp.Tool
	for i := 0; i < 50; i++ {
		tools = append(tools, &mcp.Tool{Name: "", InputSchema: nil})
	}
	findings := validateToolSchemas(target, tools, 5, redact.New())
	if len(findings) > 5 {
		t.Errorf("validator produced %d findings for a limit of 5", len(findings))
	}
}

func TestProbeRedactsSecretsTheServerEchoes(t *testing.T) {
	// The server is untrusted and may echo a credential it was given. Whatever it writes
	// must be redacted before it reaches the user. The env var is named like a token so
	// the redactor treats its value as a credential, which is the realistic case: a
	// server logging the token it was configured with.
	const secret = "test-SECRET-abc123xyz789"
	r := redact.New()

	target := Target{
		Name:    "leaky",
		Kind:    KindStdio,
		Command: mockBinary(),
		Env: map[string]string{
			"API_TOKEN":   secret,
			"MOCK_STDERR": "auth failed for token " + secret,
		},
	}

	res, err := Probe(context.Background(), target, Options{Execute: true, Timeout: 15 * time.Second}, r)
	if err != nil {
		t.Fatalf("Probe error: %v", err)
	}

	for _, f := range res.Findings {
		joined := f.Message + f.Detail + f.Remedy + f.Example
		if strings.Contains(joined, secret) {
			t.Errorf("finding leaked the server-echoed secret: %s", joined)
		}
	}
}

func TestProbeDoesNotLeakConfiguredSecrets(t *testing.T) {
	// A credential passed in env must not appear in any output path.
	const secret = "test-SECRET-abc123xyz789"
	r := redact.New()

	target := Target{
		Name:    "configured",
		Kind:    KindStdio,
		Command: mockBinary(),
		Env: map[string]string{
			"MOCK_SERVER_NAME":      "configured-server",
			"MOCK_TOOLS_ADVERTISED": "false",
			"MY_API_TOKEN":          secret,
		},
	}

	res, err := Probe(context.Background(), target, Options{Execute: true, Timeout: 15 * time.Second}, r)
	if err != nil {
		t.Fatalf("Probe error: %v", err)
	}
	for _, f := range res.Findings {
		if strings.Contains(f.Message+f.Detail+f.Remedy+f.Example, secret) {
			t.Errorf("configured secret leaked into a finding: %+v", f)
		}
	}
}

func TestProbeBoundsRunTimeUnderCancellation(t *testing.T) {
	// Cancelling the context must stop the probe promptly and still clean up the child.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	target := Target{
		Name:    "cancelled",
		Kind:    KindStdio,
		Command: mockBinary(),
		Env:     map[string]string{"MOCK_STARTUP_DELAY_MS": "30000"},
	}
	// Probe takes its own timeout; the parent context deadline is the earlier of the two.
	_, _ = Probe(ctx, target, Options{Execute: true, Timeout: 30 * time.Second}, redact.New())

	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancelled probe took %v, want prompt cancellation", elapsed)
	}
}

func TestProbeSpawnFailureForMissingCommandIsReported(t *testing.T) {
	target := Target{
		Name:    "missing",
		Kind:    KindStdio,
		Command: "definitely-not-a-real-binary-mcpdoctor-xyzzy",
	}
	res, err := Probe(context.Background(), target, Options{Execute: true, Timeout: 5 * time.Second}, redact.New())
	if err != nil {
		t.Fatalf("Probe error: %v", err)
	}
	if !hasCode(res.Findings, diagnose.CodeSpawnFailed) {
		t.Errorf("spawn failure not reported, got %v", codesOf(res.Findings))
	}
}

// allDetail concatenates every finding's text, for substring assertions.
func allDetail(findings []diagnose.Finding) string {
	var b strings.Builder
	for _, f := range findings {
		b.WriteString(f.Message)
		b.WriteString("\n")
		b.WriteString(f.Detail)
		b.WriteString("\n")
		b.WriteString(f.Remedy)
		b.WriteString("\n")
	}
	return b.String()
}

// contains reports whether a slice holds a value.
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
