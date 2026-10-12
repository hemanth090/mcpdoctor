package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
)

// fixedTime is a deterministic timestamp so report output can be asserted literally.
var fixedTime = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

// sampleInput builds a report input with one finding of each severity.
func sampleInput(findings []diagnose.Finding) Input {
	return Input{
		Report:  diagnose.Report{Findings: findings, Checked: 2},
		Sources: []Source{{Client: "claude-desktop", Path: "/home/a/.claude.json", Scope: "user", Format: "json", Exists: true, Read: true, ServerCt: 1}},
		Servers: []Server{{Name: "demo", Transport: "stdio", Probed: true, NegotiatedVersion: "2026-07-28", ServerName: "demo", ServerVersion: "1.0.0", Capabilities: []string{"tools"}, ToolCount: 2}},
		Version: "1.0.0",
		FailOn:  diagnose.SeverityError,
		Now:     fixedTime,
	}
}

// mixedFindings returns one finding at each severity.
func mixedFindings() []diagnose.Finding {
	return []diagnose.Finding{
		{Code: diagnose.CodeServerNoTransport, Severity: diagnose.SeverityError, Target: "broken", Message: "server defines neither a command nor a url", Remedy: "Add a command or a url."},
		{Code: diagnose.CodeUnknownField, Severity: diagnose.SeverityWarning, Target: "odd", Message: "unrecognised field", Remedy: "Check the spelling."},
		{Code: diagnose.CodeServerDisabled, Severity: diagnose.SeverityInfo, Target: "off", Message: "server is disabled", Remedy: "Enable it if expected."},
	}
}

func TestRenderHumanShowsEveryFindingField(t *testing.T) {
	findings := []diagnose.Finding{{
		Code:     diagnose.CodeCommandNotFound,
		Severity: diagnose.SeverityError,
		Target:   "filesystem",
		Location: "/tmp/.mcp.json:4:5",
		Message:  "command \"npx\" was not found on PATH",
		Detail:   "The client resolves a bare name against PATH.",
		Remedy:   "Install it or use an absolute path.",
		Example:  "\"command\": \"/usr/local/bin/npx\"",
	}}

	var buf bytes.Buffer
	if err := RenderHuman(&buf, sampleInput(findings), redact.New()); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"MDC1006", "filesystem", "/tmp/.mcp.json:4:5",
		"was not found on PATH", "Install it or use an absolute path.",
		`"command": "/usr/local/bin/npx"`, "ERROR (1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHumanGroupsFindingsBySeverityWithErrorsFirst(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHuman(&buf, sampleInput(mixedFindings()), redact.New()); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	out := buf.String()

	// A user scanning the report must meet the errors before the warnings.
	errIdx := strings.Index(out, "ERROR (1)")
	warnIdx := strings.Index(out, "WARNING (1)")
	infoIdx := strings.Index(out, "INFO (1)")
	if errIdx < 0 || warnIdx < 0 || infoIdx < 0 {
		t.Fatalf("missing a severity section\n%s", out)
	}
	if !(errIdx < warnIdx && warnIdx < infoIdx) {
		t.Errorf("severities out of order: error=%d warning=%d info=%d", errIdx, warnIdx, infoIdx)
	}
}

func TestRenderHumanReportsConfigurationLocationsAndProbedServers(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHuman(&buf, sampleInput(nil), redact.New()); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	out := buf.String()

	// The user needs to know what was examined, otherwise "no problems" is ambiguous.
	for _, want := range []string{
		"/home/a/.claude.json", "2026-07-28", "demo", "tools", "No problems found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderHumanIsDeterministicApartFromTheTimestamp(t *testing.T) {
	in := sampleInput(mixedFindings())

	var first bytes.Buffer
	if err := RenderHuman(&first, in, redact.New()); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	for i := 0; i < 10; i++ {
		var next bytes.Buffer
		if err := RenderHuman(&next, in, redact.New()); err != nil {
			t.Fatalf("RenderHuman: %v", err)
		}
		if next.String() != first.String() {
			t.Fatal("human output differed between identical runs")
		}
	}
}

// The security-critical test: nothing a redactor knows about may appear in output.
func TestRenderHumanRedactsRegisteredSecrets(t *testing.T) {
	const secret = "test-SECRET-abc123xyz789"
	r := redact.New()
	r.AddEnv("MY_TOKEN", secret)

	findings := []diagnose.Finding{{
		Code:     diagnose.CodeSecretInConfig,
		Severity: diagnose.SeverityWarning,
		Target:   "srv",
		Message:  "a credential was found",
		Detail:   "the server echoed " + secret + " back to us",
		Remedy:   "rotate it",
		Example:  `"MY_TOKEN": "` + secret + `"`,
	}}

	var buf bytes.Buffer
	if err := RenderHuman(&buf, sampleInput(findings), r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("human renderer leaked a registered secret:\n%s", buf.String())
	}
}

func TestRenderJSONRedactsEveryField(t *testing.T) {
	const secret = "test-SECRET-abc123xyz789"
	r := redact.New()
	r.AddEnv("MY_TOKEN", secret)

	in := sampleInput([]diagnose.Finding{{
		Code:     diagnose.CodeSecretInConfig,
		Severity: diagnose.SeverityError,
		Target:   "srv",
		Location: "/tmp/c.json:2:3",
		Message:  "found " + secret,
		Detail:   "detail " + secret,
		Remedy:   "remedy " + secret,
		Example:  "example " + secret,
	}})
	// Put the secret in a source path and an error string too, so every string field of
	// the document is exercised rather than just the finding fields.
	in.Sources = []Source{{
		Client: "c", Path: "/tmp/" + secret + ".json", Scope: "user",
		Format: "json", Exists: true, Error: "could not read " + secret,
	}}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, in, r); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("JSON renderer leaked a registered secret:\n%s", buf.String())
	}

	// Redaction must not break the document: it still has to parse.
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("JSON output is invalid after redaction: %v", err)
	}
}

func TestRenderJSONProducesTheDocumentedSchema(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleInput(mixedFindings()), redact.New()); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	// These field names are the compatibility contract; renaming one breaks consumers.
	for _, key := range []string{"schemaVersion", "tool", "toolVersion", "generatedAt", "summary", "sources", "servers", "findings"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("JSON document missing required key %q", key)
		}
	}
	if doc["schemaVersion"] != SchemaVersion {
		t.Errorf("schemaVersion = %v, want %q", doc["schemaVersion"], SchemaVersion)
	}
	if doc["tool"] != "mcp-doctor" {
		t.Errorf("tool = %v, want mcp-doctor", doc["tool"])
	}

	summary, ok := doc["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary is %T, want an object", doc["summary"])
	}
	if summary["errors"] != float64(1) || summary["warnings"] != float64(1) || summary["infos"] != float64(1) {
		t.Errorf("summary counts wrong: %v", summary)
	}
}

func TestRenderJSONIsValidJSONForEveryFindingShape(t *testing.T) {
	// Newlines and quotes in message text are the usual way hand-built reports break a
	// JSON encoder. Using an indented encoder here is about readability; using one that
	// escapes correctly is about correctness.
	awkward := []diagnose.Finding{{
		Code:     diagnose.CodeUnknownField,
		Severity: diagnose.SeverityWarning,
		Target:   "a \"quoted\" name",
		Message:  "line one\nline two\ttabbed",
		Detail:   "contains a backslash \\ and a quote \" and a </script> tag",
		Remedy:   "multi\nline\nremedy",
	}}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleInput(awkward), redact.New()); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var doc struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(doc.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(doc.Findings))
	}
	if doc.Findings[0].Message != awkward[0].Message {
		t.Errorf("message round-tripped as %q, want %q", doc.Findings[0].Message, awkward[0].Message)
	}
}

func TestBuildReportSortsFindingsDeterministically(t *testing.T) {
	// Same severity, differing codes: sorting must be by code so the output is stable.
	findings := []diagnose.Finding{
		{Code: "MDC2000", Severity: diagnose.SeverityError},
		{Code: "MDC1000", Severity: diagnose.SeverityError},
		{Code: "MDC1001", Severity: diagnose.SeverityInfo},
		{Code: "MDC1002", Severity: diagnose.SeverityWarning},
	}
	rep := BuildReport(sampleInput(findings))

	want := []string{"MDC1000", "MDC2000", "MDC1002", "MDC1001"}
	for i, code := range want {
		if rep.Findings[i].Code != code {
			t.Errorf("Findings[%d] = %q, want %q", i, rep.Findings[i].Code, code)
		}
	}
}

func TestBuildReportCountsSeverities(t *testing.T) {
	rep := BuildReport(sampleInput(mixedFindings()))
	if rep.Summary.Errors != 1 || rep.Summary.Warnings != 1 || rep.Summary.Infos != 1 {
		t.Errorf("summary = %+v, want 1/1/1", rep.Summary)
	}
}

func TestBuildReportFormatsTimestampAsUTC(t *testing.T) {
	// A local-time timestamp would make two machines produce byte-different reports,
	// which defeats diffing them in CI.
	in := sampleInput(nil)
	in.Now = time.Date(2026, 3, 1, 14, 5, 0, 0, time.FixedZone("CET", 3600))
	rep := BuildReport(in)
	if rep.GeneratedAt != "2026-03-01T13:05:00Z" {
		t.Errorf("GeneratedAt = %q, want the UTC-normalised form", rep.GeneratedAt)
	}
}

func TestRenderHumanHandlesEmptyReport(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHuman(&buf, sampleInput(nil), redact.New()); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	// Nothing found is a legitimate, meaningful outcome and must not read as a crash.
	if !strings.Contains(buf.String(), "No problems found") {
		t.Errorf("empty report output:\n%s", buf.String())
	}
}

func TestRenderHumanWithNoSourcesAtAllExplainsItself(t *testing.T) {
	in := sampleInput(nil)
	in.Sources = nil
	in.Servers = nil

	var buf bytes.Buffer
	if err := RenderHuman(&buf, in, redact.New()); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No MCP configuration found") {
		t.Errorf("output should distinguish 'nothing to check' from 'all clean':\n%s", out)
	}
}

func TestRenderHumanStatesTheExitCodeItComputed(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderHuman(&buf, sampleInput(mixedFindings()), redact.New()); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	out := buf.String()
	// Showing the code inline means a user in CI can see it without re-running anything.
	if !strings.Contains(out, "Exit code: 1") {
		t.Errorf("output does not state the exit code:\n%s", out)
	}
}

func TestIndentBlockKeepsMultiLineDetailReadable(t *testing.T) {
	got := indentBlock("first\nsecond")
	if got != "  first\n  second" {
		t.Errorf("indentBlock = %q", got)
	}
}

func TestWriteJSONValueProducesIndentedJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSONValue(&buf, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("WriteJSONValue: %v", err)
	}
	if !strings.Contains(buf.String(), "\n  ") {
		t.Errorf("output is not indented: %q", buf.String())
	}
}
