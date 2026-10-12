// Package cli implements the MCP Doctor command-line interface.
//
// It parses flags, wires the independent diagnostic packages together, maps results to
// an exit code, and nothing else. All diagnostic logic lives in internal/config and
// internal/protocol, so the CLI can be replaced or tested without touching them.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/mcpdoctor/mcpdoctor/internal/config"
	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/protocol"
	"github.com/mcpdoctor/mcpdoctor/internal/render"
)

// Version is the tool version, overridden at build time with -ldflags.
var Version = "dev"

// Env abstracts the process environment for testing.
//
// It exists so that tests can drive the interactive consent path without a terminal,
// and so that "is there a human there?" is a decision the CLI makes explicitly rather
// than one hidden inside a library call.
type Env struct {
	// Stdin is where a confirmation prompt is read from.
	Stdin io.Reader

	// Stdout receives report output.
	Stdout io.Writer

	// Stderr receives prompts, usage, and operational errors.
	Stderr io.Writer

	// IsTerminal reports whether stdin is an interactive terminal. When false the tool
	// must never prompt, because nobody is there to answer.
	IsTerminal bool
}

// DefaultEnv returns an Env wired to the real process.
func DefaultEnv() Env {
	return Env{
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		IsTerminal: isTerminal(os.Stdin),
	}
}

// run carries per-invocation state.
//
// Exit code and flags live on the run rather than in package variables so that two
// invocations in one process cannot observe each other's state, which is the same reason
// `go test -race` is a quality gate rather than a nicety.
type run struct {
	env Env

	timeout time.Duration

	// exitSet and exitCode carry the result of a completed diagnostic run back to Main.
	// A diagnostic run is not a command failure, so it does not surface as a Go error.
	exitSet  bool
	exitCode int
}

// Main runs the CLI and returns a process exit code.
//
// It never calls os.Exit, so the whole program is testable in-process. The caller in
// cmd/mcpdoctor is the only place that terminates the process.
func Main(ctx context.Context, env Env, args []string) int {
	r := &run{env: env, timeout: protocol.DefaultTimeout}

	root := r.newRootCommand()
	root.SetArgs(args)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		// A diagnostic run records its exit code directly rather than returning an
		// error, so anything arriving here is a usage or internal failure.
		if r.exitSet {
			return r.exitCode
		}
		fmt.Fprintf(env.Stderr, "mcp-doctor: %v\n", err)
		return diagnose.ExitUsage
	}
	if r.exitSet {
		return r.exitCode
	}
	return diagnose.ExitOK
}

// Options are the flags shared by the diagnostic commands.
type Options struct {
	ConfigPaths []string
	ProjectRoot string
	JSON        bool
	FailOn      string

	// Execute authorises starting configured servers. Without it, probe is a no-op
	// beyond static validation.
	Execute bool

	// AllowRemote authorises contacting a server that is not on this machine.
	AllowRemote bool

	// Yes records that consent was given deliberately, for non-interactive use.
	Yes bool

	// Server narrows a probe to one named server.
	Server string
}

// newRootCommand builds the command tree.
func (r *run) newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "mcp-doctor",
		Short: "Diagnose MCP server configurations and running servers",
		Long: `MCP Doctor validates Model Context Protocol client configurations and,
with explicit authorisation, probes running servers.

It never modifies your configuration and never contacts the network unless you tell it
to. Running a configured command is always an explicit, separate opt-in.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		r.newCheckCommand(),
		r.newProbeCommand(),
		r.newClientsCommand(),
		r.newDocsCommand(),
	)
	return root
}

// newCheckCommand builds the static analysis command.
//
// Check performs zero execution and zero network access. That is the whole reason it is
// a separate command rather than a flag on probe: the guarantee is structural, not a
// policy that a later change could weaken.
func (r *run) newCheckCommand() *cobra.Command {
	opts := &Options{}
	var quiet bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate MCP client configurations without running anything",
		Long: `Check validates every MCP client configuration it can find.

It reads configuration files and reports problems. It does not start a server, does not
open a network connection, and does not modify any file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			threshold, err := diagnose.ParseSeverity(opts.FailOn)
			if err != nil {
				return err
			}
			result := runCheck(cmd.Context(), *opts)
			return r.emit(result, threshold)
		},
	}

	r.bindCommonFlags(cmd, opts)
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress progress notes; findings are always printed")
	return cmd
}

// newProbeCommand builds the live protocol command.
func (r *run) newProbeCommand() *cobra.Command {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:   "probe [server]",
		Short: "Start configured servers and diagnose the MCP protocol conversation",
		Long: `Probe starts each configured server, completes the MCP initialization
handshake, and validates the server's advertised capabilities and tools.

Running a server means running someone else's code. MCP Doctor will not do it without
explicit authorisation: pass --execute, and confirm the command when prompted. In a
non-interactive context such as CI, --execute must be paired with --yes so that the
authorisation is deliberate and visible in the script.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.Server = args[0]
			}
			threshold, err := diagnose.ParseSeverity(opts.FailOn)
			if err != nil {
				return err
			}
			result := runProbe(cmd.Context(), r, *opts)
			return r.emit(result, threshold)
		},
	}

	r.bindCommonFlags(cmd, opts)
	cmd.Flags().BoolVar(&opts.Execute, "execute", false, "authorise starting the configured servers")
	cmd.Flags().BoolVar(&opts.AllowRemote, "allow-remote", false, "authorise contacting servers that are not on this machine")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false, "skip the confirmation prompt; required with --execute when stdin is not a terminal")
	return cmd
}

// bindCommonFlags registers the flags shared across diagnostic commands.
func (r *run) bindCommonFlags(cmd *cobra.Command, opts *Options) {
	cmd.Flags().StringSliceVar(&opts.ConfigPaths, "config", nil, "path to a specific configuration file (repeatable)")
	cmd.Flags().StringVarP(&opts.ProjectRoot, "project", "C", "", "directory to scan for project-level configurations (default: current directory)")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "output a versioned JSON report instead of human-readable text")
	cmd.Flags().StringVar(&opts.FailOn, "fail-on", "error", "lowest severity that fails the run: error, warning, or info")
	cmd.Flags().DurationVarP(&r.timeout, "timeout", "t", protocol.DefaultTimeout, "bound on a single server probe")
}

// newClientsCommand lists the configuration paths MCP Doctor knows about.
func (r *run) newClientsCommand() *cobra.Command {
	var projectRoot string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "clients",
		Short: "List the MCP client configuration paths that were checked",
		Long: `Clients lists every configuration location MCP Doctor knows about on this
platform, and whether a file was found there.

This reads nothing but the filesystem and is the fastest way to confirm that MCP Doctor
is looking where you think it is.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sources := config.DescribePaths(projectRoot)
			sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })

			if asJSON {
				return writeJSON(r.env.Stdout, clientList(sources))
			}

			fmt.Fprintln(r.env.Stdout, "Configuration locations checked by MCP Doctor:")
			fmt.Fprintln(r.env.Stdout)
			for _, s := range sources {
				status := "not found"
				if _, err := os.Stat(s.Path); err == nil {
					status = "present"
				}
				fmt.Fprintf(r.env.Stdout, "  %-9s %-8s %s\n", status, s.Scope, s.Path)
			}
			fmt.Fprintf(r.env.Stdout, "\n%d location(s) checked. Pass --project DIR to include project-level configurations.\n", len(sources))
			return nil
		},
	}

	cmd.Flags().StringVarP(&projectRoot, "project", "C", "", "directory to scan for project-level configurations")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

// clientLocation is the JSON shape of one discovered configuration path.
type clientLocation struct {
	Client string `json:"client"`
	Path   string `json:"path"`
	Scope  string `json:"scope"`
	Format string `json:"format"`
	Exists bool   `json:"exists"`
}

// clientList builds the JSON payload for the clients command.
func clientList(sources []config.Source) []clientLocation {
	out := make([]clientLocation, 0, len(sources))
	for _, s := range sources {
		_, err := os.Stat(s.Path)
		out = append(out, clientLocation{
			Client: string(s.Client),
			Path:   s.Path,
			Scope:  string(s.Scope),
			Format: string(s.Format),
			Exists: err == nil,
		})
	}
	return out
}

// newDocsCommand prints the documented exit codes.
func (r *run) newDocsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "docs",
		Short: "Show the documented exit codes and how to use them in scripts",
		Long: `Docs prints the exit-code contract, which is stable and safe to branch on
from a CI pipeline or a shell script.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(r.env.Stdout, exitCodeHelp)
			return nil
		},
	}
}

// exitCodeHelp is the documented exit-code contract.
//
// It lives in the binary as well as the documentation, because a user who cannot find
// the documentation should still be able to run `mcp-doctor docs`.
const exitCodeHelp = `Exit codes

  0   The run completed and no finding reached the --fail-on threshold.
  1   The run completed and at least one finding reached the threshold. Your
      configuration has problems; MCP Doctor did not fail.
  2   MCP Doctor could not do its job: a bad flag, an unreadable input, or an
      internal error. Distinct from 1 so a pipeline can tell a broken config
      from a broken tool.
  130 The run was cancelled with Ctrl-C.

Examples

  # Fail a CI job when a configuration has errors
  mcp-doctor check --config .mcp.json

  # Be stricter than the default
  mcp-doctor check --config .mcp.json --fail-on warning

  # Machine-readable output for further processing
  mcp-doctor check --json | jq '.findings[] | select(.severity == "error")'

  # Probe a local server, answering the consent prompt interactively
  mcp-doctor probe my-server --execute

  # Non-interactive: both flags are required, so the authorisation is explicit
  mcp-doctor probe --execute --yes
`

// writeJSON encodes a value as indented JSON.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// emit writes the report in the requested format and records the exit code.
//
// Recording rather than returning is deliberate: a configuration with errors is a
// successful diagnosis, not a failure of the program. Conflating the two is how a tool
// teaches users to ignore its exit code.
func (r *run) emit(result runResult, threshold diagnose.Severity) error {
	input := render.Input{
		Report:  result.report,
		Sources: result.sources,
		Servers: result.servers,
		Version: Version,
		FailOn:  threshold,
		Now:     time.Now(),
	}

	var err error
	if result.json {
		err = render.RenderJSON(r.env.Stdout, input, result.redactor)
	} else {
		err = render.RenderHuman(r.env.Stdout, input, result.redactor)
	}
	if err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	r.exitSet = true
	r.exitCode = result.report.ExitCodeFor(threshold)
	return nil
}

// ensure the render package stays referenced even if a renderer is swapped out.
var _ = render.SchemaVersion
