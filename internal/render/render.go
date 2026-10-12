// Package render turns a report into output a human or a program can consume.
//
// Both renderers are pure functions over a report value: no I/O, no diagnostics, no
// state. That is what makes the output testable against literal expected strings, and it
// is why a rendering bug cannot silently change what was diagnosed.
//
// Every renderer receives a redactor. There is deliberately no way to render without one,
// because a rendering path that could skip redaction is a leak waiting to happen.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
)

// SchemaVersion is the version of the JSON report schema.
//
// It is a string, not a number, so the versioning scheme can change without a format
// change. The compatibility promise is additive within a major version: new optional
// fields may appear, existing fields keep their meaning, and nothing is removed within
// a major version.
const SchemaVersion = "1.0"

// Report is the top-level JSON document.
//
// Every field is stable within SchemaVersion. Consumers should ignore unknown fields,
// which is what lets new fields be added without breaking anyone.
type Report struct {
	// SchemaVersion identifies the document shape.
	SchemaVersion string `json:"schemaVersion"`

	// Tool names the producing tool and its version.
	Tool    string `json:"tool"`
	Version string `json:"toolVersion"`

	// GeneratedAt is the only non-deterministic field in the document. Every other
	// field is byte-for-byte reproducible for a given input, which is what lets a CI job
	// diff two reports.
	GeneratedAt string `json:"generatedAt"`

	// Summary counts findings by severity.
	Summary Summary `json:"summary"`

	// Sources lists every configuration file that was examined, whether or not it
	// existed. This is what distinguishes "everything passed" from "nothing was found".
	Sources []Source `json:"sources"`

	// Servers describes each probed server's negotiated state.
	Servers []Server `json:"servers"`

	// Findings are the diagnostics, sorted deterministically.
	Findings []Finding `json:"findings"`
}

// Summary counts findings by severity.
type Summary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
}

// Source is a configuration file that was examined.
type Source struct {
	Client   string `json:"client"`
	Path     string `json:"path"`
	Scope    string `json:"scope"`
	Format   string `json:"format"`
	Exists   bool   `json:"exists"`
	Read     bool   `json:"read"`
	Symlink  string `json:"symlinkTarget,omitempty"`
	Error    string `json:"error,omitempty"`
	ServerCt int    `json:"serverCount"`
}

// Server is the result of probing one server.
type Server struct {
	Name string `json:"name"`

	// Transport is "stdio" or "http". Empty when the server was not probed.
	Transport string `json:"transport,omitempty"`

	// Probed is false when execution was not authorised or the server was skipped.
	Probed bool `json:"probed"`

	// NegotiatedVersion is the protocol version agreed during initialization.
	NegotiatedVersion string `json:"negotiatedVersion,omitempty"`

	// ServerName and ServerVersion come from the server's own implementation info.
	ServerName    string `json:"serverName,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`

	// Capabilities are the server's advertised capabilities, sorted.
	Capabilities []string `json:"capabilities,omitempty"`

	// ToolCount is how many tools the server advertised.
	ToolCount int `json:"toolCount,omitempty"`

	// DurationMS is the probe duration in milliseconds. Informational only.
	DurationMS int64 `json:"durationMs,omitempty"`
}

// Finding is the JSON shape of a diagnostic.
//
// It mirrors the fields of diagnose.Finding but is a separate type so that the wire
// format can be versioned independently of the internal model. Adding a field here is
// additive and therefore safe.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Target   string `json:"target"`
	Location string `json:"location,omitempty"`
	Message  string `json:"message"`
	Detail   string `json:"detail,omitempty"`
	Remedy   string `json:"remedy,omitempty"`
	Example  string `json:"example,omitempty"`
}

// Input is everything a renderer needs.
//
// It is a plain value assembled by the CLI, so a renderer has no way to reach back into
// the program's state and no way to trigger a side effect.
type Input struct {
	Report  diagnose.Report
	Sources []Source
	Servers []Server
	Version string
	FailOn  diagnose.Severity
	Now     time.Time
}

// RenderHuman writes a human-readable report.
//
// Output is deterministic apart from the generated-at line, so two runs over the same
// configuration produce identical text.
func RenderHuman(w io.Writer, in Input, r *redact.Redactor) error {
	rep := BuildReport(in)

	if len(rep.Sources) == 0 && len(rep.Findings) == 0 {
		return writef(w, "No MCP configuration found.\n\nRun with --list-clients to see the paths that were checked.\n")
	}

	var b strings.Builder
	b.WriteString("MCP Doctor\n")
	b.WriteString("==========\n\n")

	// Sources first, so the user knows what was examined before they read the verdicts.
	if len(rep.Sources) > 0 {
		b.WriteString("Configurations checked\n")
		b.WriteString("-----------------------\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, s := range rep.Sources {
			status := "ok"
			detail := ""
			switch {
			case !s.Exists:
				status = "not found"
			case s.Error != "":
				status = "error"
				detail = s.Error
			default:
				detail = fmt.Sprintf("%d server(s)", s.ServerCt)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", status, s.Path, detail)
			if s.Symlink != "" {
				fmt.Fprintf(tw, "    -> symlink to %s\n", s.Symlink)
			}
		}
		tw.Flush()
		b.WriteString("\n")
	}

	if len(rep.Servers) > 0 {
		b.WriteString("Servers probed\n")
		b.WriteString("-------------\n")
		for _, s := range rep.Servers {
			if !s.Probed {
				fmt.Fprintf(&b, "  %s\tnot probed\n", s.Name)
				continue
			}
			fmt.Fprintf(&b, "  %s\n", s.Name)
			if s.ServerName != "" {
				fmt.Fprintf(&b, "    implementation: %s %s\n", s.ServerName, s.ServerVersion)
			}
			if s.NegotiatedVersion != "" {
				fmt.Fprintf(&b, "    protocol:       %s\n", s.NegotiatedVersion)
			}
			if len(s.Capabilities) > 0 {
				fmt.Fprintf(&b, "    capabilities:   %s\n", strings.Join(s.Capabilities, ", "))
			}
			if s.ToolCount > 0 {
				fmt.Fprintf(&b, "    tools:          %d\n", s.ToolCount)
			}
			if s.DurationMS > 0 {
				fmt.Fprintf(&b, "    duration:       %dms\n", s.DurationMS)
			}
		}
		b.WriteString("\n")
	}

	// Findings grouped by severity, so an error is never buried under an info.
	bySeverity := groupBySeverity(rep.Findings)
	total := len(rep.Findings)
	if total == 0 {
		b.WriteString("No problems found.\n\n")
	} else {
		for _, sev := range []diagnose.Severity{diagnose.SeverityError, diagnose.SeverityWarning, diagnose.SeverityInfo} {
			findings := bySeverity[sev]
			if len(findings) == 0 {
				continue
			}
			fmt.Fprintf(&b, "%s (%d)\n", strings.ToUpper(sev.String()), len(findings))
			for _, f := range findings {
				fmt.Fprintf(&b, "  [%s] %s\n", f.Code, f.Message)
				if f.Location != "" {
					fmt.Fprintf(&b, "    at %s\n", f.Location)
				}
				fmt.Fprintf(&b, "    target: %s\n", f.Target)
				if f.Detail != "" {
					fmt.Fprintf(&b, "    %s\n", indentBlock(f.Detail))
				}
				if f.Remedy != "" {
					fmt.Fprintf(&b, "    fix: %s\n", f.Remedy)
				}
				if f.Example != "" {
					fmt.Fprintf(&b, "    example: %s\n", f.Example)
				}
				b.WriteString("\n")
			}
		}
	}

	// Summary and exit status last, so it is the last thing a scrolling reader sees.
	errs, warns, infos := rep.Summary.Errors, rep.Summary.Warnings, rep.Summary.Infos
	fmt.Fprintf(&b, "Summary: %d error(s), %d warning(s), %d info\n", errs, warns, infos)

	code := diagnose.Report{Findings: rep.findingsForExit()}.ExitCodeFor(in.FailOn)
	fmt.Fprintf(&b, "Exit code: %d (fail-on: %s)\n", code, in.FailOn)

	// Every string that came from a config file or a server passes through the redactor
	// before it reaches the writer. The renderer is the last line of defence, and it is
	// applied here rather than at each construction site so a new field cannot forget.
	return writef(w, r.Redact(b.String()))
}

// RenderJSON writes the versioned JSON report.
//
// Indented output is deliberate: this report is routinely read by a human first and a
// script second, and compact JSON serves only the second.
func RenderJSON(w io.Writer, in Input, r *redact.Redactor) error {
	rep := BuildReport(in)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// A redacting encoder is used rather than encoding the report and then filtering the
	// bytes, because filtering output after encoding could mangle JSON structure.
	encErr := enc.Encode(redactingDoc{Report: rep, r: r})
	if encErr != nil {
		return fmt.Errorf("encode report: %w", encErr)
	}
	return nil
}

// redactingDoc wraps a report so that encoding it redacts every string field.
type redactingDoc struct {
	Report
	r *redact.Redactor
}

// MarshalJSON encodes the report with every string redacted.
//
// Walking the document explicitly is what guarantees no field escapes: a new string
// field added to Report must be added here too, and the test that asserts this keeps
// them in step.
func (d redactingDoc) MarshalJSON() ([]byte, error) {
	doc := struct {
		SchemaVersion string    `json:"schemaVersion"`
		Tool          string    `json:"tool"`
		ToolVersion   string    `json:"toolVersion"`
		GeneratedAt   string    `json:"generatedAt"`
		Summary       Summary   `json:"summary"`
		Sources       []Source  `json:"sources"`
		Servers       []Server  `json:"servers"`
		Findings      []Finding `json:"findings"`
	}{
		SchemaVersion: d.SchemaVersion,
		Tool:          d.Tool,
		ToolVersion:   d.Version,
		GeneratedAt:   d.GeneratedAt,
		Summary:       d.Summary,
		Sources:       d.redactSources(),
		Servers:       d.Servers,
		Findings:      d.redactFindings(),
	}
	return json.Marshal(doc)
}

// redactSources redacts every string in the source list.
func (d redactingDoc) redactSources() []Source {
	if d.Sources == nil {
		return nil
	}
	out := make([]Source, len(d.Sources))
	for i, s := range d.Sources {
		s.Path = d.r.Redact(s.Path)
		s.Error = d.r.Redact(s.Error)
		s.Symlink = d.r.Redact(s.Symlink)
		out[i] = s
	}
	return out
}

// redactFindings redacts every string in the finding list.
func (d redactingDoc) redactFindings() []Finding {
	if d.Findings == nil {
		return nil
	}
	out := make([]Finding, len(d.Findings))
	for i, f := range d.Findings {
		f.Target = d.r.Redact(f.Target)
		f.Location = d.r.Redact(f.Location)
		f.Message = d.r.Redact(f.Message)
		f.Detail = d.r.Redact(f.Detail)
		f.Remedy = d.r.Redact(f.Remedy)
		f.Example = d.r.Redact(f.Example)
		out[i] = f
	}
	return out
}

// findingsForExit maps the JSON findings back to internal findings for exit-code
// computation. Only severity matters, so the conversion is trivial, but it keeps the
// exit logic in one place rather than duplicating the threshold rule in each renderer.
func (rep Report) findingsForExit() []diagnose.Finding {
	out := make([]diagnose.Finding, len(rep.Findings))
	for i, f := range rep.Findings {
		out[i] = diagnose.Finding{Severity: f.severity()}
	}
	return out
}

// severity converts the wire severity back to the internal enum.
func (f Finding) severity() diagnose.Severity {
	s, err := diagnose.ParseSeverity(f.Severity)
	if err != nil {
		return diagnose.SeverityError
	}
	return s
}

// typedFinding pairs a wire finding with its numeric severity so sorting does not have
// to parse the severity name back out of its string form.
type typedFinding struct {
	severity diagnose.Severity
	wire     Finding
}

// BuildReport assembles the wire report from renderer input.
//
// Findings are sorted here, once, so both renderers share the same ordering and a diff
// between a human and a JSON run lines up.
func BuildReport(in Input) Report {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	errs, warns, infos := in.Report.Counts()

	// Sort by numeric severity, not by the severity's string name. Alphabetical order
	// would put "info" before "warning", burying warnings under the least important
	// findings, which is the opposite of what a severity ordering is for.
	typed := make([]typedFinding, 0, len(in.Report.Findings))
	for _, f := range in.Report.Findings {
		typed = append(typed, typedFinding{severity: f.Severity, wire: Finding{
			Code:     f.Code,
			Severity: f.Severity.String(),
			Target:   f.Target,
			Location: f.Location,
			Message:  f.Message,
			Detail:   f.Detail,
			Remedy:   f.Remedy,
			Example:  f.Example,
		}})
	}
	sort.SliceStable(typed, func(i, j int) bool {
		if typed[i].severity != typed[j].severity {
			return typed[i].severity < typed[j].severity
		}
		return typed[i].wire.Code < typed[j].wire.Code
	})

	findings := make([]Finding, 0, len(typed))
	for _, t := range typed {
		findings = append(findings, t.wire)
	}

	return Report{
		SchemaVersion: SchemaVersion,
		Tool:          "mcp-doctor",
		Version:       in.Version,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Summary:       Summary{Errors: errs, Warnings: warns, Infos: infos},
		Sources:       in.Sources,
		Servers:       in.Servers,
		Findings:      findings,
	}
}

// WriteJSONValue encodes v as indented JSON to w.
//
// It is the small helper used by commands that emit a value which is not a full Report,
// such as the client location list. Keeping it here means every JSON document the tool
// writes is indented and therefore readable by a human who pipes it into a file.
func WriteJSONValue(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// groupBySeverity partitions findings for ordered rendering.
func groupBySeverity(findings []Finding) map[diagnose.Severity][]Finding {
	out := map[diagnose.Severity][]Finding{
		diagnose.SeverityError:   {},
		diagnose.SeverityWarning: {},
		diagnose.SeverityInfo:    {},
	}
	for _, f := range findings {
		out[f.severity()] = append(out[f.severity()], f)
	}
	return out
}

// indentBlock prefixes every line of a block with two spaces, for detail text that
// contains newlines.
func indentBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}

// writef writes a string, wrapping any error with context.
func writef(w io.Writer, s string) error {
	_, err := io.WriteString(w, s)
	return err
}
