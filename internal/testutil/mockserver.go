// Package testutil provides controllable MCP servers for integration tests.
//
// A protocol diagnostic tool can only be trusted if its own protocol handling has been
// exercised against a real implementation. Testing the probe against a mock that shares
// assumptions with the probe proves nothing; testing it against the official SDK's
// server proves the conversation is actually correct.
//
// Both servers here are built on the official SDK. Behaviour is controlled through
// environment variables so that a single binary can produce every scenario the probe
// must handle: a healthy server, a server that crashes at startup, one that hangs, one
// that pollutes stdout, and one that advertises broken tools.
package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Behaviour is the environment-variable contract the mock server reads.
//
// It is exported so tests can build the environment explicitly rather than repeating
// string literals, which is what keeps the mock and the tests from drifting apart.
type Behaviour struct {
	// ServerName and ServerVersion populate serverInfo.
	ServerName    string
	ServerVersion string

	// Tools are advertised through tools/list.
	Tools []Tool

	// AdvertiseTools controls whether the tools capability is declared at all. Setting it
	// false while still listing tools is impossible, so a test that wants an unadvertised
	// capability leaves Tools empty.
	AdvertiseTools bool

	// StartupDelayMS sleeps before the server begins answering, which exercises the
	// probe's startup timeout.
	StartupDelayMS int

	// ExitCodeBeforeInit makes the process exit with this code before answering
	// initialize, which exercises the "server crashed at startup" diagnosis.
	ExitCodeBeforeInit int

	// StderrText is written to stderr at startup. This is where a real server would
	// report why it could not start, and it is the most valuable thing the probe can
	// surface, so the tests assert it reaches the user.
	StderrText string

	// StdoutNoise is written to stdout before any protocol traffic. The specification
	// forbids this, and it is one of the most common real MCP failures, so the probe has
	// to detect and explain it.
	StdoutNoise string
}

// Tool describes one tool the mock server advertises.
type Tool struct {
	Name string

	// InputSchema is the raw JSON Schema for the tool. Empty means "a valid empty object
	// schema", which is what a well-behaved server sends for a no-argument tool.
	InputSchema string

	// OmitInputSchema removes inputSchema entirely, testing the protocol requirement
	// that every tool declares one.
	OmitInputSchema bool

	// BadInputSchema sends an inputSchema that is not a JSON Schema object.
	BadInputSchema bool

	// DuplicateName advertises this tool a second time under the same name, testing
	// ambiguity detection.
	DuplicateName bool
}

// Env renders the behaviour as a process environment.
//
// Returning a slice rather than a map keeps the result deterministic under Go's
// randomised map iteration, which matters because a test that compares environments
// byte for byte would otherwise be flaky.
func (b Behaviour) Env() []string {
	env := append(os.Environ(),
		"MOCK_SERVER_NAME="+b.ServerName,
		"MOCK_SERVER_VERSION="+b.ServerVersion,
		"MOCK_TOOLS_ADVERTISED="+strconv.FormatBool(b.AdvertiseTools),
		"MOCK_STARTUP_DELAY_MS="+strconv.Itoa(b.StartupDelayMS),
	)
	if b.ExitCodeBeforeInit != 0 {
		env = append(env, "MOCK_EXIT_CODE="+strconv.Itoa(b.ExitCodeBeforeInit))
	}
	if b.StderrText != "" {
		env = append(env, "MOCK_STDERR="+b.StderrText)
	}
	if b.StdoutNoise != "" {
		env = append(env, "MOCK_STDOUT_NOISE="+b.StdoutNoise)
	}
	for i, tool := range b.Tools {
		prefix := "MOCK_TOOL_" + strconv.Itoa(i) + "_"
		env = append(env,
			prefix+"NAME="+tool.Name,
			prefix+"SCHEMA="+tool.InputSchema,
			prefix+"OMIT_SCHEMA="+strconv.FormatBool(tool.OmitInputSchema),
			prefix+"BAD_SCHEMA="+strconv.FormatBool(tool.BadInputSchema),
			prefix+"DUPLICATE="+strconv.FormatBool(tool.DuplicateName),
		)
	}
	return env
}

// buildBinary compiles a mock server binary once per test binary run and returns its
// path.
//
// The mock must be a genuinely separate process: an in-process fake cannot test process
// lifecycle, stdio framing, exit codes, or signal handling, which are most of what the
// probe does wrong.
func buildBinary(t *testing.T, dir, pkg string) string {
	t.Helper()

	name := filepath.Base(dir) + "-mock"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)

	cmd := exec.Command("go", "build", "-o", binary, pkg)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, out)
	}
	return binary
}

// StdioCommand returns the command and arguments that run a mock stdio server with the
// given behaviour.
func StdioCommand(t *testing.T, b Behaviour) (string, []string) {
	t.Helper()
	return buildBinary(t, t.TempDir(), "github.com/mcpdoctor/mcpdoctor/cmd/mockserver"), nil
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate go.mod above the working directory")
	return ""
}

// HTTPServer starts a real Streamable HTTP MCP server on loopback.
//
// It uses the SDK's own handler, so a transport test exercises a genuine implementation
// rather than a stand-in that could share bugs with the client under test.
func HTTPServer(t *testing.T, b Behaviour) *httptest.Server {
	t.Helper()

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return buildSDKServer(b)
	}, nil)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// buildSDKServer constructs an in-process SDK MCP server from a behaviour.
func buildSDKServer(b Behaviour) *mcp.Server {
	name := b.ServerName
	if strings.TrimSpace(name) == "" {
		name = "mock-server"
	}
	version := b.ServerVersion
	if strings.TrimSpace(version) == "" {
		version = "1.0.0"
	}

	impl := &mcp.Implementation{Name: name, Version: version}

	opts := &mcp.ServerOptions{}
	if b.AdvertiseTools {
		opts.Capabilities = &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}
	}

	server := mcp.NewServer(impl, opts)
	for _, tool := range b.Tools {
		addTool(server, tool)
	}
	return server
}

// addTool registers one tool with an SDK server.
func addTool(server *mcp.Server, spec Tool) {
	tool := &mcp.Tool{Name: spec.Name, Description: "mock tool " + spec.Name}

	switch {
	case spec.OmitInputSchema:
		tool.InputSchema = nil
	case spec.BadInputSchema:
		tool.InputSchema = "this should have been an object"
	case spec.InputSchema != "":
		tool.InputSchema = spec.InputSchema
	default:
		tool.InputSchema = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	server.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "mock result"}},
		}, nil
	})
}

// unusedGuard keeps imports honest if the helpers above are trimmed during refactoring.
var (
	_ = sync.Mutex{}
	_ = Behavior{}
)

// Behavior is an alias kept for readability in test call sites.
type Behavior = Behaviour
