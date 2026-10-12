// Command mockserver is a controllable MCP server used by MCP Doctor's integration
// tests.
//
// It is a real MCP server built on the official SDK, configured entirely through
// environment variables, so a test can reproduce every failure mode the probe must
// diagnose: a healthy server, one that exits before initializing, one that hangs, one
// that writes non-protocol text to stdout, and one that advertises malformed tools.
//
// It is not part of the shipped binary. It exists so that the probe's protocol handling
// is tested against a real implementation rather than against a mock that could share
// the probe's own mistakes.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolSpec is one tool read from the environment.
type toolSpec struct {
	Name        string
	InputSchema string
	OmitSchema  bool
	BadSchema   bool
	Duplicate   bool
}

// main runs the mock server described by the environment.
func main() {
	// Reproduce the two startup behaviours the probe must distinguish. They happen
	// before any protocol interaction so that the probe sees exactly what a real server
	// would produce in the same situation.
	if text := os.Getenv("MOCK_STDERR"); text != "" {
		fmt.Fprintln(os.Stderr, text)
	}
	if noise := os.Getenv("MOCK_STDOUT_NOISE"); noise != "" {
		// Deliberately written to stdout. The specification reserves stdout for protocol
		// messages, so this is exactly the mistake the probe must detect.
		fmt.Fprintln(os.Stdout, noise)
	}
	if code := os.Getenv("MOCK_EXIT_CODE"); code != "" {
		n, err := strconv.Atoi(code)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mock server: bad MOCK_EXIT_CODE %q\n", code)
			os.Exit(2)
		}
		// Exit before the server is created, so nothing ever answers initialize.
		os.Exit(n)
	}
	if delay := os.Getenv("MOCK_STARTUP_DELAY_MS"); delay != "" {
		ms, err := strconv.Atoi(delay)
		if err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}

	server := buildServer()
	transport := &mcp.StdioTransport{}

	// Connect never returns for a well-behaved stdio server; it serves until stdin
	// closes. Cancelling the context on stdin closure gives the process a clean exit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect returns once the server is wired up; the session then serves messages in
	// the background until the transport closes, at which point Wait returns and we exit
	// cleanly. That mirrors how a real stdio server behaves, and lets the probe's
	// shutdown ladder be tested against something well behaved.
	session, err := server.Connect(ctx, transport, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mock server connect:", err)
		os.Exit(1)
	}

	// Wait for the client to close the connection. A real server blocks for its whole
	// lifetime here; returning when the session ends models the normal case where the
	// client closes stdin and the server shuts down.
	if err := session.Wait(); err != nil {
		// A non-nil error on shutdown is not interesting to the probe, which is already
		// collecting its diagnostics by this point.
		os.Exit(1)
	}
}

// buildServer constructs the server described by the environment variables.
func buildServer() *mcp.Server {
	name := envOr("MOCK_SERVER_NAME", "mock-server")
	version := envOr("MOCK_SERVER_VERSION", "1.0.0")

	impl := &mcp.Implementation{Name: name, Version: version}

	tools, advertised := readTools()
	opts := &mcp.ServerOptions{}
	if advertised {
		opts.Capabilities = &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}
	}

	server := mcp.NewServer(impl, opts)
	for _, spec := range tools {
		addTool(server, spec)
	}
	return server
}

// readTools reconstructs the tool list from indexed environment variables.
//
// Tools are encoded with an index prefix rather than as a JSON blob so that a value
// containing arbitrary JSON stays intact through the environment, where quoting rules
// vary between platforms.
func readTools() ([]toolSpec, bool) {
	var tools []toolSpec
	for i := 0; ; i++ {
		prefix := "MOCK_TOOL_" + strconv.Itoa(i) + "_"
		name, ok := os.LookupEnv(prefix + "NAME")
		if !ok {
			break
		}
		tools = append(tools, toolSpec{
			Name:        name,
			InputSchema: os.Getenv(prefix + "SCHEMA"),
			OmitSchema:  envBool(prefix + "OMIT_SCHEMA"),
			BadSchema:   envBool(prefix + "BAD_SCHEMA"),
			Duplicate:   envBool(prefix + "DUPLICATE"),
		})
	}

	advertised := tools != nil
	if v := os.Getenv("MOCK_TOOLS_ADVERTISED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			advertised = parsed
		}
	}
	return tools, advertised
}

// addTool registers one tool, including its duplicate when requested.
func addTool(server *mcp.Server, spec toolSpec) {
	register(server, spec)
	if spec.Duplicate {
		// A second registration under the same name models a server that built its tool
		// list by concatenating two sources and collided.
		register(server, spec)
	}
}

// register adds a single tool definition to the server.
func register(server *mcp.Server, spec toolSpec) {
	tool := &mcp.Tool{Name: spec.Name, Description: "mock tool " + spec.Name}

	switch {
	case spec.OmitSchema:
		tool.InputSchema = nil
	case spec.BadSchema:
		tool.InputSchema = "not-a-json-schema-object"
	case strings.TrimSpace(spec.InputSchema) != "":
		tool.InputSchema = spec.InputSchema
	default:
		tool.InputSchema = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	server.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "mock result for " + spec.Name}},
		}, nil
	})
}

// envOr returns an environment value or a fallback.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// envBool reads a boolean environment variable.
func envBool(name string) bool {
	v, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && v
}
