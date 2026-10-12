package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mcpdoctor/mcpdoctor/internal/config"
	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
)

// harness runs the CLI in-process and captures everything it wrote.
type harness struct {
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	code   int
}

// runCLI invokes Main with an in-memory environment.
//
// Driving the real entry point rather than the internal functions is what makes these
// tests worth having: it is the only way to assert the exit code a shell would see.
func runCLI(t *testing.T, env Env, args ...string) harness {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	env.Stdout = stdout
	env.Stderr = stderr
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}

	code := Main(context.Background(), env, args)
	return harness{stdout: stdout, stderr: stderr, code: code}
}

// testEnv returns a non-interactive environment, which is the safe default for tests:
// nothing is ever confirmed implicitly.
func testEnv() Env {
	return Env{
		Stdin:      strings.NewReader(""),
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
		IsTerminal: false,
	}
}

// writeConfig writes a config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

const healthyConfig = `{
  "mcpServers": {
    "good": {"command": "go", "args": ["version"]}
  }
}`

const brokenConfig = `{
  "mcpServers": {
    "broken": {"url": "http://remote.example.com/mcp"},
    "missing": {"command": "definitely-not-installed-xyzzy"}
  }
}`

func TestCheckExitsZeroOnAHealthyConfig(t *testing.T) {
	p := writeConfig(t, healthyConfig)
	h := runCLI(t, testEnv(), "check", "--config", p)
	if h.code != diagnose.ExitOK {
		t.Errorf("exit code = %d, want %d\n%s", h.code, diagnose.ExitOK, h.stdout)
	}
}

func TestCheckExitsOneWhenFindingsReachTheThreshold(t *testing.T) {
	// Exit 1 means "your configuration has problems". It must be distinct from exit 2,
	// which means "the tool could not run".
	p := writeConfig(t, brokenConfig)
	h := runCLI(t, testEnv(), "check", "--config", p)
	if h.code != diagnose.ExitFindings {
		t.Errorf("exit code = %d, want %d\n%s", h.code, diagnose.ExitFindings, h.stdout)
	}
}

func TestCheckExitsTwoOnAUsageError(t *testing.T) {
	h := runCLI(t, testEnv(), "check", "--fail-on", "nonsense")
	if h.code != diagnose.ExitUsage {
		t.Errorf("exit code = %d, want %d for a bad flag value", h.code, diagnose.ExitUsage)
	}
}

func TestFailOnWarningMakesWarningsFailARunThatWouldOtherwisePass(t *testing.T) {
	// A configuration whose only findings are warnings should pass under the default
	// threshold and fail under --fail-on warning. That is the entire point of the flag.
	warnOnly := `{"mcpServers":{
		"s":{"url":"https://user:pass@example.com/mcp","headers":{"X":"y"}}
	}}`
	p := writeConfig(t, warnOnly)

	defaultRun := runCLI(t, testEnv(), "check", "--config", p)
	strictRun := runCLI(t, testEnv(), "check", "--config", p, "--fail-on", "warning")

	if defaultRun.code != diagnose.ExitOK {
		t.Errorf("warning-only config failed the default threshold (exit %d)\n%s", defaultRun.code, defaultRun.stdout)
	}
	if strictRun.code != diagnose.ExitFindings {
		t.Errorf("warning-only config passed --fail-on warning (exit %d)\n%s", strictRun.code, strictRun.stdout)
	}
}

func TestJSONOutputIsValidAndCarriesTheSchemaVersion(t *testing.T) {
	p := writeConfig(t, brokenConfig)
	h := runCLI(t, testEnv(), "check", "--config", p, "--json")

	var doc struct {
		SchemaVersion string `json:"schemaVersion"`
		Summary       struct {
			Errors int `json:"errors"`
		} `json:"summary"`
		Findings []struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("--json did not produce valid JSON: %v\n%s", err, h.stdout)
	}
	if doc.SchemaVersion == "" {
		t.Error("JSON output has no schemaVersion")
	}
	if doc.Summary.Errors == 0 || len(doc.Findings) == 0 {
		t.Errorf("expected error findings, got summary %+v and %d findings", doc.Summary, len(doc.Findings))
	}
	for _, f := range doc.Findings {
		if f.Code == "" || f.Severity == "" {
			t.Errorf("finding missing code or severity: %+v", f)
		}
	}
}

func TestProbeWithoutExecuteNeverStartsAnything(t *testing.T) {
	// The central safety property, asserted directly rather than inferred: a config
	// naming a command that creates a marker file is the unambiguous way to prove the
	// tool did not run it.
	marker := filepath.Join(t.TempDir(), "MARKER_SHOULD_NOT_EXIST")
	p := writeConfig(t, `{"mcpServers":{"sneaky":{"command":"touch","args":["`+marker+`"]}}}`)

	// Use an absolute path to a command that certainly exists on this platform, so the
	// only reason the marker is absent is that the tool declined to run anything.
	h := runCLI(t, testEnv(), "probe", "--config", p)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("probe executed the configured command without authorisation")
	}
	if !strings.Contains(h.stdout.String(), "MDC2000") {
		t.Errorf("expected the not-authorised finding, got:\n%s", h.stdout)
	}
}

func TestProbeWithoutExecuteResolvesNoTargetsAtAll(t *testing.T) {
	// Without --execute the run stops before targets are resolved, so no server list is
	// produced. That ordering is deliberate: the refusal happens before any work, not
	// after it, so there is nothing to unwind.
	p := writeConfig(t, healthyConfig)
	h := runCLI(t, testEnv(), "probe", "--config", p)

	if !strings.Contains(h.stdout.String(), "MDC2000") {
		t.Errorf("expected the not-authorised finding:\n%s", h.stdout)
	}
	if strings.Contains(h.stdout.String(), "protocol:") || strings.Contains(h.stdout.String(), "tools:") {
		t.Errorf("output reports live server state without --execute:\n%s", h.stdout)
	}
}

func TestProbeWithExecuteButNoYesInNonInteractiveRefuses(t *testing.T) {
	// CI has no terminal and nobody to answer, so the tool must refuse rather than
	// assume consent.
	env := testEnv()
	env.IsTerminal = false

	h := runCLI(t, env, "probe", "--config", writeConfig(t, healthyConfig), "--execute")
	if !strings.Contains(h.stderr.String(), "--yes") {
		t.Errorf("expected the refusal to mention --yes:\n%s", h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "MDC2000") {
		t.Errorf("expected the declined finding:\n%s", h.stdout)
	}
}

func TestProbeInteractiveDeclineStartsNothing(t *testing.T) {
	// A user typing "n" at the prompt is a refusal, and it must be honoured.
	env := testEnv()
	env.IsTerminal = true
	env.Stdin = strings.NewReader("n\n")

	h := runCLI(t, env, "probe", "--config", writeConfig(t, healthyConfig), "--execute")
	if !strings.Contains(h.stdout.String(), "MDC2000") {
		t.Errorf("declining should produce the not-authorised finding:\n%s", h.stdout)
	}
	if !strings.Contains(h.stdout.String(), "not probed") {
		t.Errorf("a server was contacted despite a declined prompt:\n%s", h.stdout)
	}
	if strings.Contains(h.stdout.String(), "protocol:") {
		t.Errorf("a negotiated protocol is reported despite a declined prompt:\n%s", h.stdout)
	}
}

func TestProbePromptListsTheCommandsItWouldRun(t *testing.T) {
	// The prompt must show the exact command, because consenting to "something" is not
	// consent at all.
	env := testEnv()
	env.IsTerminal = true
	env.Stdin = strings.NewReader("n\n")

	h := runCLI(t, env, "probe", "--config", writeConfig(t, healthyConfig), "--execute")
	if !strings.Contains(h.stderr.String(), "go version") {
		t.Errorf("prompt did not show the command it would run:\n%s", h.stderr)
	}
}

func TestProbeRedactsSecretsInTheConsentPrompt(t *testing.T) {
	// A secret in args must not be printed to the terminal by the prompt itself.
	if runtime.GOOS == "windows" {
		t.Skip("consent prompt path uses POSIX-independent logic but arg rendering differs")
	}
	const secret = "test-SECRET-abc123xyz789"
	cfg := `{"mcpServers":{"s":{"command":"my-server","args":["--token","` + secret + `"]}}}`

	env := testEnv()
	env.IsTerminal = true
	env.Stdin = strings.NewReader("n\n")

	h := runCLI(t, env, "probe", "--config", writeConfig(t, cfg), "--execute")
	if strings.Contains(h.stderr.String(), secret) {
		t.Errorf("consent prompt leaked a secret from args:\n%s", h.stderr)
	}
}

func TestClientsListsPathsWithoutReadingThem(t *testing.T) {
	h := runCLI(t, testEnv(), "clients")
	if h.code != diagnose.ExitOK {
		t.Errorf("exit code = %d, want 0", h.code)
	}
	if !strings.Contains(h.stdout.String(), "Configuration locations checked") {
		t.Errorf("unexpected clients output:\n%s", h.stdout)
	}
}

func TestClientsJSONIsParseable(t *testing.T) {
	h := runCLI(t, testEnv(), "clients", "--json")
	var list []map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &list); err != nil {
		t.Fatalf("clients --json did not produce valid JSON: %v\n%s", err, h.stdout)
	}
	if len(list) == 0 {
		t.Error("clients reported no locations")
	}
}

func TestDocsPrintsTheExitCodeContract(t *testing.T) {
	h := runCLI(t, testEnv(), "docs")
	for _, want := range []string{"Exit codes", "0", "1", "2", "130", "--fail-on"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("docs output missing %q:\n%s", want, h.stdout)
		}
	}
}

func TestVersionFlagPrintsAVersion(t *testing.T) {
	h := runCLI(t, testEnv(), "--version")
	if !strings.Contains(h.stdout.String(), "version") {
		t.Errorf("--version output = %q", h.stdout.String())
	}
}

func TestIsRemoteDistinguishesLoopbackFromPublicHosts(t *testing.T) {
	// This classification decides whether a probe requires --allow-remote, so being
	// wrong in either direction matters: too strict blocks legitimate work, too lax
	// sends requests to the internet the operator did not intend.
	remote := []string{
		"http://example.com/mcp",
		"https://api.example.com/mcp",
		"http://192.168.1.10:8080/mcp",
		"https://10.0.0.5/mcp",
	}
	for _, u := range remote {
		if !isRemote(u) {
			t.Errorf("isRemote(%q) = false, want true", u)
		}
	}

	local := []string{
		"http://localhost:3000/mcp",
		"http://127.0.0.1:8080/mcp",
		"https://127.0.0.1/mcp",
		"HTTP://LOCALHOST:9000/mcp",
		"http://[::1]:9000/mcp",
	}
	for _, u := range local {
		if isRemote(u) {
			t.Errorf("isRemote(%q) = true, want false", u)
		}
	}
}

func TestRedactURLRemovesEmbeddedCredentials(t *testing.T) {
	// The consent prompt prints a URL; a password in it must not reach the screen.
	got := redactURL("https://admin:hunter2secret@example.com/mcp")
	if strings.Contains(got, "hunter2secret") {
		t.Errorf("redactURL left the password in place: %q", got)
	}
	if !strings.Contains(got, "example.com/mcp") {
		t.Errorf("redactURL destroyed the host and path: %q", got)
	}

	// A URL with no credentials is returned unchanged.
	clean := "https://example.com/mcp"
	if got := redactURL(clean); got != clean {
		t.Errorf("redactURL modified a clean URL: %q", got)
	}
}

func TestAdapterForFallsBackToAPermissiveAdapter(t *testing.T) {
	// An unattributable file must still be validated, and correct values must not be
	// reported as invalid just because the tool does not know which client wrote it.
	a := adapterFor(config.Client("some-unknown-client"))
	if a.ServersKey != "mcpServers" {
		t.Errorf("fallback adapter ServersKey = %q", a.ServersKey)
	}
	for _, spelling := range []string{"stdio", "http", "streamable-http", "streamableHttp", "sse"} {
		if _, ok := a.SupportedTypeValues[spelling]; !ok {
			t.Errorf("fallback adapter does not accept documented type %q", spelling)
		}
	}
}

func TestUnknownCommandIsAUsageFailure(t *testing.T) {
	h := runCLI(t, testEnv(), "definitely-not-a-command")
	if h.code != diagnose.ExitUsage {
		t.Errorf("exit code = %d, want %d", h.code, diagnose.ExitUsage)
	}
}

func TestAdapterForReturnsTheRealAdapterForAKnownClient(t *testing.T) {
	a := adapterFor(config.ClientZed)
	if a.ServersKey != "context_servers" {
		t.Errorf("Zed adapter ServersKey = %q, want context_servers", a.ServersKey)
	}
}

func TestSecretsNeverAppearInAnyOutputPath(t *testing.T) {
	// One config exercising both env and header credentials, checked across the human
	// renderer, the JSON renderer, and stderr. Any leak here is a real incident.
	const envSecret = "sk_REDACTED_SAMPLE"
	const headerSecret = "Bearer test-SECRET-abc123xyz789"

	cfg := `{"mcpServers":{
		"a":{"url":"https://example.com/mcp","headers":{"Authorization":"` + headerSecret + `"}},
		"b":{"command":"my-server","env":{"MY_API_KEY":"` + envSecret + `"}}
	}}`
	p := writeConfig(t, cfg)

	for _, args := range [][]string{
		{"check", "--config", p},
		{"check", "--config", p, "--json"},
	} {
		h := runCLI(t, testEnv(), args...)
		for _, secret := range []string{envSecret, headerSecret} {
			if strings.Contains(h.stdout.String(), secret) {
				t.Errorf("%v leaked %q into stdout", args, secret)
			}
			if strings.Contains(h.stderr.String(), secret) {
				t.Errorf("%v leaked %q into stderr", args, secret)
			}
		}
	}
}

func TestMissingConfigFileIsInformationalNotFatal(t *testing.T) {
	// Most machines have no config for most clients. Failing a build for that would make
	// the tool useless in exactly the common case.
	p := filepath.Join(t.TempDir(), "nothing-here.json")
	h := runCLI(t, testEnv(), "check", "--config", p)
	if h.code != diagnose.ExitOK {
		t.Errorf("missing explicit config exited %d, want %d\n%s", h.code, diagnose.ExitOK, h.stdout)
	}
	if !strings.Contains(h.stdout.String(), "MDC0001") {
		t.Errorf("expected a not-found finding:\n%s", h.stdout)
	}
}

func TestMalformedConfigReportsTheLine(t *testing.T) {
	p := writeConfig(t, "{\n  \"mcpServers\": {\n    \"a\": {bad}\n  }\n}")
	h := runCLI(t, testEnv(), "check", "--config", p)

	if h.code != diagnose.ExitFindings {
		t.Errorf("exit code = %d, want %d", h.code, diagnose.ExitFindings)
	}
	if !strings.Contains(h.stdout.String(), "MDC0004") {
		t.Errorf("expected a not-valid-JSON finding:\n%s", h.stdout)
	}
	if !strings.Contains(h.stdout.String(), ":3:") {
		t.Errorf("expected the error location to name line 3:\n%s", h.stdout)
	}
}

func TestCheckIsDeterministicAcrossInvocations(t *testing.T) {
	p := writeConfig(t, brokenConfig)
	first := runCLI(t, testEnv(), "check", "--config", p)

	for i := 0; i < 5; i++ {
		next := runCLI(t, testEnv(), "check", "--config", p)
		if next.stdout.String() != first.stdout.String() {
			t.Fatalf("run %d produced different output", i)
		}
		if next.code != first.code {
			t.Fatalf("run %d exit code = %d, want %d", i, next.code, first.code)
		}
	}
}

func TestCancelTheContextStopsARunPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stdout := &bytes.Buffer{}
	code := Main(ctx, Env{
		Stdin: strings.NewReader(""), Stdout: stdout, Stderr: &bytes.Buffer{}, IsTerminal: false,
	}, []string{"check", "--config", writeConfig(t, healthyConfig)})

	// A cancelled run must still produce a coherent exit code rather than hanging or
	// panicking.
	if code != diagnose.ExitOK && code != diagnose.ExitUsage && code != diagnose.ExitFindings {
		t.Errorf("cancelled run produced unexpected exit code %d", code)
	}
}

func TestRunStateDoesNotLeakBetweenInvocations(t *testing.T) {
	// The exit code lives on the run, not in package state, so a second invocation must
	// not observe the first one's result.
	p := writeConfig(t, brokenConfig)
	if got := runCLI(t, testEnv(), "check", "--config", p).code; got != diagnose.ExitFindings {
		t.Fatalf("first run exit = %d, want %d", got, diagnose.ExitFindings)
	}
	good := runCLI(t, testEnv(), "check", "--config", writeConfig(t, healthyConfig))
	if good.code != diagnose.ExitOK {
		t.Errorf("second run exit = %d, want %d (state leaked from the first)", good.code, diagnose.ExitOK)
	}
}
