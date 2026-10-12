package config

import (
	"strings"
	"testing"

	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
)

// parseFixture parses a config body for the given client and returns its servers.
func parseFixture(t *testing.T, client Client, body string) ([]Server, []diagnose.Finding) {
	t.Helper()
	a, ok := AdapterFor(client)
	if !ok {
		t.Fatalf("no adapter for client %q", client)
	}
	node, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("fixture is not valid JSON: %v\n%s", err, body)
	}
	return ParseServers(a, node)
}

// validateFixture runs the full validation over a config body.
func validateFixture(t *testing.T, client Client, body string) []diagnose.Finding {
	t.Helper()
	servers, parseFindings := parseFixture(t, client, body)
	all := append([]diagnose.Finding{}, parseFindings...)
	all = append(all, ValidateAll(mustAdapter(t, client), servers, redact.New())...)
	return all
}

func mustAdapter(t *testing.T, c Client) Adapter {
	t.Helper()
	a, ok := AdapterFor(c)
	if !ok {
		t.Fatalf("no adapter for client %q", c)
	}
	return a
}

// codesOf returns the set of diagnostic codes in a finding list, for terse assertions.
func codesOf(findings []diagnose.Finding) map[string]bool {
	set := make(map[string]bool, len(findings))
	for _, f := range findings {
		set[f.Code] = true
	}
	return set
}

// hasCode reports whether any finding carries the given code.
func hasCode(findings []diagnose.Finding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// findCode returns the first finding with the given code.
func findCode(findings []diagnose.Finding, code string) (diagnose.Finding, bool) {
	for _, f := range findings {
		if f.Code == code {
			return f, true
		}
	}
	return diagnose.Finding{}, false
}

// assertNoSecretLeak fails if any synthetic test secret appears in a finding.
func assertNoSecretLeak(t *testing.T, findings []diagnose.Finding, secret string) {
	t.Helper()
	for _, f := range findings {
		joined := f.Message + f.Detail + f.Remedy + f.Example + f.Location
		if strings.Contains(joined, secret) {
			t.Errorf("finding %s leaked the secret value:\n%q", f.Code, joined)
		}
	}
}

func TestParseServersReadsStandardMcpServersShape(t *testing.T) {
	servers, findings := parseFixture(t, ClientClaudeDesktop, `{
		"mcpServers": {
			"filesystem": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]},
			"remote": {"url": "https://example.com/mcp"}
		}
	}`)
	if len(findings) != 0 {
		t.Fatalf("unexpected parse findings: %+v", findings)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(servers))
	}
	// Servers are returned sorted by name for deterministic output.
	if servers[0].Name != "filesystem" || servers[1].Name != "remote" {
		t.Errorf("servers not sorted by name: %q, %q", servers[0].Name, servers[1].Name)
	}
	if servers[0].Transport != TransportStdio {
		t.Errorf("filesystem transport = %q, want stdio", servers[0].Transport)
	}
	if servers[1].Transport != TransportHTTP {
		t.Errorf("remote transport = %q, want http", servers[1].Transport)
	}
}

func TestZedUsesContextServersKey(t *testing.T) {
	// Zed stores MCP servers under context_servers, not mcpServers. An adapter that
	// ignored that would report every Zed user as having nothing configured.
	servers, _ := parseFixture(t, ClientZed, `{
		"context_servers": {
			"local": {"command": "some-command", "args": ["arg-1"]}
		}
	}`)
	if len(servers) != 1 || servers[0].Name != "local" {
		t.Fatalf("Zed context_servers not parsed: %+v", servers)
	}
}

func TestVSCodeAcceptsBothServersAndMcpServersKeys(t *testing.T) {
	for _, key := range []string{"servers", "mcpServers"} {
		t.Run(key, func(t *testing.T) {
			servers, _ := parseFixture(t, ClientVSCode, `{"`+key+`": {"a": {"type": "stdio", "command": "npx"}}}`)
			if len(servers) != 1 {
				t.Errorf("key %q not recognised: %+v", key, servers)
			}
		})
	}
}

func TestWindsurfPrefersServerUrlOverUrl(t *testing.T) {
	servers, _ := parseFixture(t, ClientWindsurf, `{
		"mcpServers": {"r": {"serverUrl": "https://example.com/mcp"}}
	}`)
	if len(servers) != 1 {
		t.Fatalf("got %d servers", len(servers))
	}
	if servers[0].URL != "https://example.com/mcp" {
		t.Errorf("URL = %q, want the serverUrl value", servers[0].URL)
	}
}

func TestGeminiCLIInfersTransportFromWhichKeyIsPresent(t *testing.T) {
	// Gemini CLI has no type field. The key that is present selects the transport, and
	// the three spellings mean three different things.
	tests := []struct {
		name string
		body string
		want TransportKind
	}{
		{"command means stdio", `{"mcpServers":{"a":{"command":"npx"}}}`, TransportStdio},
		{"httpUrl means streamable http", `{"mcpServers":{"a":{"httpUrl":"https://example.com/mcp"}}}`, TransportHTTP},
		{"bare url means legacy sse", `{"mcpServers":{"a":{"url":"https://example.com/sse"}}}`, TransportSSE},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			servers, _ := parseFixture(t, ClientGeminiCLI, tc.body)
			if len(servers) != 1 {
				t.Fatalf("got %d servers", len(servers))
			}
			if servers[0].Transport != tc.want {
				t.Errorf("transport = %q, want %q (resolution: %s)", servers[0].Transport, tc.want, servers[0].Resolution)
			}
		})
	}
}

func TestClineAndRooUseDifferentSpellingsForTheSameTransport(t *testing.T) {
	// The single most surprising cross-client difference, and exactly the kind of thing
	// a hardcoded check would get wrong.
	cline, _ := parseFixture(t, ClientCline, `{"mcpServers":{"a":{"type":"streamableHttp","url":"https://example.com/mcp"}}}`)
	if len(cline) != 1 || cline[0].Transport != TransportHTTP {
		t.Errorf("Cline camelCase streamableHttp not recognised: %+v", cline)
	}
	roo, _ := parseFixture(t, ClientRooCode, `{"mcpServers":{"a":{"type":"streamable-http","url":"https://example.com/mcp"}}}`)
	if len(roo) != 1 || roo[0].Transport != TransportHTTP {
		t.Errorf("Roo kebab-case streamable-http not recognised: %+v", roo)
	}
}

func TestServerWithNoTransportIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"broken":{}}}`)
	if !hasCode(findings, diagnose.CodeServerNoTransport) {
		t.Fatalf("missing CodeServerNoTransport: %+v", findings)
	}
	f, _ := findCode(findings, diagnose.CodeServerNoTransport)
	if f.Severity != diagnose.SeverityError {
		t.Errorf("severity = %v, want error", f.Severity)
	}
	if f.Remedy == "" {
		t.Error("finding has no remedy")
	}
}

func TestServerWithBothTransportsIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"both":{"command":"npx","url":"https://example.com/mcp"}}}`)
	if !hasCode(findings, diagnose.CodeServerBothTransports) {
		t.Fatalf("missing CodeServerBothTransports: %+v", findings)
	}
}

func TestEmptyCommandIsReported(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"   "}}}`)
	if !hasCode(findings, diagnose.CodeServerNoTransport) {
		t.Errorf("whitespace-only command should read as no transport: %+v", findings)
	}
}

func TestCommandWithShellSyntaxIsRejected(t *testing.T) {
	// A pasted shell line is a common mistake and must not be treated as a bare
	// executable name, because the resulting error message would be baffling.
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"npx -y @scope/pkg"}}}`)
	if !hasCode(findings, diagnose.CodeCommandContainsSeparator) {
		t.Fatalf("pasted command line not detected: %+v", findings)
	}
}

func TestCommandWithSemicolonIsRejected(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"curl http://x | sh"}}}`)
	if !hasCode(findings, diagnose.CodeCommandContainsSeparator) {
		t.Errorf("shell pipeline in command not detected: %+v", findings)
	}
}

func TestUnknownCommandOnPathIsAnError(t *testing.T) {
	// A name that certainly does not exist, so the assertion does not depend on what is
	// installed on the machine running the test.
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"definitely-not-a-real-binary-xyzzy"}}}`)
	if !hasCode(findings, diagnose.CodeCommandNotFound) {
		t.Errorf("missing executable not reported: %+v", findings)
	}
}

func TestKnownCommandOnPathProducesNoError(t *testing.T) {
	// "go" is on PATH in CI by definition; the test is that a resolvable bare command
	// is not reported as missing.
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"go","args":["version"]}}}`)
	if hasCode(findings, diagnose.CodeCommandNotFound) {
		t.Errorf("resolvable command reported as missing: %+v", findings)
	}
}

func TestArgsNotAnArrayIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"npx","args":"-y pkg"}}}`)
	if !hasCode(findings, diagnose.CodeArgsInvalid) {
		t.Errorf("string args not reported: %+v", findings)
	}
}

func TestEmptyArgumentIsAWarning(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"npx","args":["-y","","pkg"]}}}`)
	f, ok := findCode(findings, diagnose.CodeArgEmpty)
	if !ok {
		t.Fatalf("empty argument not reported: %+v", findings)
	}
	if f.Severity != diagnose.SeverityWarning {
		t.Errorf("severity = %v, want warning", f.Severity)
	}
	if !strings.Contains(f.Message, "argument 1") {
		t.Errorf("message does not identify which argument: %q", f.Message)
	}
}

func TestShellMetacharacterInArgIsAWarningNotAnError(t *testing.T) {
	// Literal metacharacters are correct argv behaviour. The finding exists to flag the
	// paste accident, not to forbid the character.
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"npx","args":["--pattern","foo|bar"]}}}`)
	f, ok := findCode(findings, diagnose.CodeArgsShellMetacharacter)
	if !ok {
		t.Fatalf("metacharacter not reported: %+v", findings)
	}
	if f.Severity != diagnose.SeverityWarning {
		t.Errorf("severity = %v, want warning", f.Severity)
	}
}

func TestInvalidEnvNameIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"npx","env":{"BAD-NAME":"v"}}}}`)
	if !hasCode(findings, diagnose.CodeEnvNameInvalid) {
		t.Errorf("invalid env name not reported: %+v", findings)
	}
}

func TestEnvInterpolationIsAWarning(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"npx","env":{"API_URL":"${SERVICE_URL}/api"}}}}`)
	f, ok := findCode(findings, diagnose.CodeEnvInterpolation)
	if !ok {
		t.Fatalf("interpolation placeholder not reported: %+v", findings)
	}
	if f.Severity != diagnose.SeverityWarning {
		t.Errorf("severity = %v, want warning", f.Severity)
	}
}

func TestMissingCwdIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"command":"npx","cwd":"/definitely/not/a/real/dir/xyzzy"}}}`)
	if !hasCode(findings, diagnose.CodeCwdInvalid) {
		t.Errorf("missing cwd not reported: %+v", findings)
	}
}

func TestInvalidURLSchemeIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"url":"ftp://example.com/mcp"}}}`)
	if !hasCode(findings, diagnose.CodeURLSchemeUnsupported) {
		t.Errorf("unsupported scheme not reported: %+v", findings)
	}
}

func TestPlainHTTPToRemoteHostIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"url":"http://example.com/mcp"}}}`)
	f, ok := findCode(findings, diagnose.CodeURLInsecure)
	if !ok {
		t.Fatalf("plain http to remote host not reported: %+v", findings)
	}
	if f.Severity != diagnose.SeverityError {
		t.Errorf("severity = %v, want error", f.Severity)
	}
}

func TestPlainHTTPToLoopbackIsAllowed(t *testing.T) {
	// Local development servers legitimately use plain http; flagging them would train
	// users to ignore the warning.
	for _, host := range []string{"http://localhost:3000/mcp", "http://127.0.0.1:8080/mcp", "http://[::1]:9000/mcp"} {
		findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"url":"`+host+`"}}}`)
		if hasCode(findings, diagnose.CodeURLInsecure) {
			t.Errorf("loopback endpoint %s flagged as insecure", host)
		}
	}
}

func TestURLCredentialsAreAWarningAndNotPrinted(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"url":"https://admin:hunter2secretvalue@example.com/mcp"}}}`)
	f, ok := findCode(findings, diagnose.CodeURLCredentials)
	if !ok {
		t.Fatalf("embedded credentials not reported: %+v", findings)
	}
	if f.Severity != diagnose.SeverityWarning {
		t.Errorf("severity = %v, want warning", f.Severity)
	}
	assertNoSecretLeak(t, findings, "hunter2secretvalue")
}

func TestURLWithoutSchemeIsAnError(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a":{"url":"example.com/mcp"}}}`)
	if !hasCode(findings, diagnose.CodeURLInvalid) {
		t.Errorf("schemeless URL not reported: %+v", findings)
	}
}

func TestUnknownTypeValueIsReportedAsTypeMismatch(t *testing.T) {
	// A typo in `type` is one of the most common real failures, and it must not be
	// silently accepted.
	findings := validateFixture(t, ClientClaudeCode, `{"mcpServers":{"a":{"type":"htp","url":"https://example.com/mcp"}}}`)
	if !hasCode(findings, diagnose.CodeTypeMismatch) {
		t.Errorf("typo in type not reported: %+v", findings)
	}
}

func TestDisabledServerIsInformational(t *testing.T) {
	findings := validateFixture(t, ClientCline, `{"mcpServers":{"a":{"command":"npx","disabled":true}}}`)
	f, ok := findCode(findings, diagnose.CodeServerDisabled)
	if !ok {
		t.Fatalf("disabled server not reported: %+v", findings)
	}
	if f.Severity != diagnose.SeverityInfo {
		t.Errorf("severity = %v, want info", f.Severity)
	}
}

func TestUnknownFieldIsAWarningNotAnError(t *testing.T) {
	// Client format drift must be visible without breaking anyone's build.
	findings := validateFixture(t, ClientCursor, `{"mcpServers":{"a":{"type":"stdio","command":"npx","brandNewField":"x"}}}`)
	f, ok := findCode(findings, diagnose.CodeUnknownField)
	if !ok {
		t.Fatalf("unknown field not reported: %+v", findings)
	}
	if f.Severity != diagnose.SeverityWarning {
		t.Errorf("severity = %v, want warning", f.Severity)
	}
}

func TestSecretInEnvIsWarnedWithoutDisclosingTheValue(t *testing.T) {
	const secret = "test-SECRET-abc123xyz789"
	body := `{"mcpServers":{"a":{"command":"npx","env":{"MY_API_KEY":"` + secret + `"}}}}`

	findings := validateFixture(t, ClientClaudeDesktop, body)
	f, ok := findCode(findings, diagnose.CodeSecretInConfig)
	if !ok {
		t.Fatalf("credential not flagged: %+v", findings)
	}
	if f.Severity != diagnose.SeverityWarning {
		t.Errorf("severity = %v, want warning", f.Severity)
	}
	assertNoSecretLeak(t, findings, secret)
}

func TestSecretInHeaderIsWarnedWithoutDisclosingTheValue(t *testing.T) {
	const secret = "Bearer test-SECRET-abc123xyz789"
	body := `{"mcpServers":{"a":{"url":"https://example.com/mcp","headers":{"Authorization":"` + secret + `"}}}}`

	findings := validateFixture(t, ClientClaudeDesktop, body)
	if !hasCode(findings, diagnose.CodeSecretInConfig) {
		t.Fatalf("credential in header not flagged: %+v", findings)
	}
	assertNoSecretLeak(t, findings, secret)
}

func TestEnvValuesAreRegisteredWithTheRedactor(t *testing.T) {
	// The validation pass is where config secrets enter the redaction system. If this
	// regresses, every later output path leaks them.
	const secret = "test-SECRET-abc123xyz789"
	a := mustAdapter(t, ClientClaudeDesktop)
	servers, _ := parseFixture(t, a.Client, `{"mcpServers":{"a":{"command":"npx","env":{"MY_TOKEN":"`+secret+`"}}}}`)

	r := redact.New()
	ValidateAll(a, servers, r)

	if got := r.Redact("server said " + secret); strings.Contains(got, secret) {
		t.Errorf("secret survived after validation registered it: %q", got)
	}
}

func TestNonObjectServerEntryIsReported(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers":{"a": "not an object"}}`)
	if !hasCode(findings, diagnose.CodeServerNotObject) {
		t.Errorf("non-object entry not reported: %+v", findings)
	}
}

func TestServersKeyNotAnObjectIsReported(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers": ["a", "b"]}`)
	if !hasCode(findings, diagnose.CodeServersNotObject) {
		t.Errorf("array mcpServers not reported: %+v", findings)
	}
}

func TestNonObjectTopLevelIsReported(t *testing.T) {
	findings := validateFixture(t, ClientClaudeDesktop, `[1, 2, 3]`)
	if !hasCode(findings, diagnose.CodeConfigJSONType) {
		t.Errorf("non-object top level not reported: %+v", findings)
	}
}

func TestConfigWithNoServersIsNotAnError(t *testing.T) {
	// Most clients ship with an empty configuration. Reporting that as broken would make
	// the tool cry wolf on every fresh install.
	findings := validateFixture(t, ClientClaudeDesktop, `{"mcpServers": {}}`)
	if len(findings) != 0 {
		t.Errorf("empty config produced findings: %+v", findings)
	}
}

func TestEveryFindingHasRemedyCodeAndTarget(t *testing.T) {
	// The product contract: an unactionable diagnostic is worse than none, so every
	// finding must carry the fields a user needs to act on.
	body := `{
		"mcpServers": {
			"a": {"command": "definitely-missing-xyz", "args": [""], "env": {"BAD-NAME": "v"}},
			"b": {"url": "http://remote.example.com/mcp"}
		}
	}`
	for _, f := range validateFixture(t, ClientClaudeDesktop, body) {
		if f.Code == "" {
			t.Errorf("finding with empty code: %+v", f)
		}
		if f.Severity.String() == "unknown" {
			t.Errorf("finding with invalid severity: %+v", f)
		}
		if f.Target == "" {
			t.Errorf("finding %s has no target", f.Code)
		}
		if f.Message == "" {
			t.Errorf("finding %s has no message", f.Code)
		}
		if f.Remedy == "" {
			t.Errorf("finding %s has no remedy", f.Code)
		}
	}
}

func TestValidationIsDeterministicAcrossRuns(t *testing.T) {
	body := `{"mcpServers":{
		"z":{"command":"missing-one-xyz"},
		"a":{"url":"http://remote.example.com/mcp"},
		"m":{"url":"http://another.example.com/mcp","headers":{"X":"y"}}
	}}`

	first := validateFixture(t, ClientClaudeDesktop, body)
	for i := 0; i < 20; i++ {
		next := validateFixture(t, ClientClaudeDesktop, body)
		if len(next) != len(first) {
			t.Fatalf("run %d produced %d findings, want %d", i, len(next), len(first))
		}
		for j := range first {
			if next[j] != first[j] {
				t.Fatalf("run %d finding %d = %+v, want %+v", i, j, next[j], first[j])
			}
		}
	}
}

func TestNonJSONFormatsAreDetectedButNotParsed(t *testing.T) {
	// Continue is YAML and Codex is TOML. Reporting them as detected-but-unsupported is
	// honest; silently ignoring them would leave users with unexplained silence.
	for _, tc := range []struct {
		client Client
		format Format
	}{
		{ClientContinue, FormatYAML},
		{ClientCodex, FormatTOML},
	} {
		a := mustAdapter(t, tc.client)
		if a.Format != tc.format {
			t.Errorf("%s format = %q, want %q", tc.client, a.Format, tc.format)
		}
		res := ReadFile(Source{Path: "/nonexistent", Format: tc.format}, DefaultLimits())
		if res.Err == nil || res.Err.Kind != FileErrUnsupportedFormat {
			t.Errorf("%s: err = %+v, want unsupported-format", tc.client, res.Err)
		}
	}
}

func TestAdaptersUseDocumentedServerKeys(t *testing.T) {
	// A regression guard on the facts the research established, so that a future edit
	// cannot quietly change a documented format.
	want := map[Client]string{
		ClientClaudeDesktop: "mcpServers",
		ClientClaudeCode:    "mcpServers",
		ClientVSCode:        "servers",
		ClientCursor:        "mcpServers",
		ClientWindsurf:      "mcpServers",
		ClientZed:           "context_servers",
		ClientCline:         "mcpServers",
		ClientRooCode:       "mcpServers",
		ClientAmazonQ:       "mcpServers",
		ClientGeminiCLI:     "mcpServers",
		ClientCopilotCLI:    "mcpServers",
	}
	for client, key := range want {
		a := mustAdapter(t, client)
		if a.ServersKey != key {
			t.Errorf("%s ServersKey = %q, want %q", client, a.ServersKey, key)
		}
	}
}
