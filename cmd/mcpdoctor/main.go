// Command mcp-doctor diagnoses Model Context Protocol client configurations and, with
// explicit authorisation, running MCP servers.
//
// It is a single static binary with no runtime dependencies beyond the platform's own
// libraries. It makes no network requests other than to an MCP endpoint the operator
// explicitly asked it to probe, and it sends no telemetry.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/mcpdoctor/mcpdoctor/internal/cli"
)

func main() {
	// A cancellation signal is wired through context so that Ctrl-C stops the run
	// promptly, including any subprocess the tool started, rather than leaving a server
	// running after the tool has exited.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Main(ctx, cli.DefaultEnv(), os.Args[1:]))
}