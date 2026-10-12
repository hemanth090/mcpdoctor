// Package diagnose defines the vocabulary MCP Doctor uses to report problems: the
// finding model, severities, stable diagnostic codes, and the mapping from a set of
// findings to a process exit code.
//
// This package is a leaf. It imports nothing from the rest of MCP Doctor so that the
// diagnostic contract can be tested without a real server, a real config file, or a
// terminal.
package diagnose

import (
	"fmt"
	"sort"
	"strings"
)

// Severity classifies how much a finding should worry the operator.
//
// The zero value is SeverityError so that a Finding built without an explicit severity
// still fails a CI gate, rather than silently passing.
type Severity int

const (
	// SeverityError means the configuration or server is broken: it will not work as
	// written, or it is a security problem the operator must resolve.
	SeverityError Severity = iota

	// SeverityWarning means something is very likely wrong but may still work, such as
	// an unusual but valid value or a field a newer client version added.
	SeverityWarning

	// SeverityInfo is purely informational. It never affects an exit code and exists so
	// that the tool can tell users something useful without alarming them.
	SeverityInfo
)

// String returns the canonical lowercase name used in output and in the JSON schema.
//
// The name is part of the public contract: it appears in the JSON report and users are
// expected to match on it.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInfo:
		return "info"
	default:
		return "unknown"
	}
}

// MarshalText implements encoding.TextMarshaler so Severity round-trips through JSON as
// its canonical name rather than its numeric value.
func (s Severity) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// ParseSeverity converts a canonical severity name back to a Severity. It exists for
// tests and for any caller that reads a severity from configuration, such as
// --fail-on.
func ParseSeverity(name string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "error":
		return SeverityError, nil
	case "warning":
		return SeverityWarning, nil
	case "info":
		return SeverityInfo, nil
	default:
		return SeverityError, fmt.Errorf("unknown severity %q: want error, warning, or info", name)
	}
}

// atLeast reports whether s is at or above the given threshold.
func (s Severity) atLeast(threshold Severity) bool { return s <= threshold }

// Finding is a single diagnostic result.
//
// Every field except Code and Severity is present because an unactionable diagnostic is
// worse than no diagnostic: a user who is told what is wrong but not where or how to
// fix it has gained nothing.
type Finding struct {
	// Code is a stable identifier such as "MDC1009". Codes are never reused or
	// renumbered: a user may have automation matching on them, and breaking that
	// silently would be worse than any single diagnostic being wrong.
	Code string `json:"code"`

	// Severity classifies the finding's urgency.
	Severity Severity `json:"severity"`

	// Target names what the finding is about: a server name, a config file path, or a
	// URL. It is never empty for a finding a user can act on.
	Target string `json:"target"`

	// Location points at the exact place in a file, formatted "path:line:col", when the
	// finding came from parsing positioned source. Empty when the finding is not tied to
	// a specific position.
	Location string `json:"location,omitempty"`

	// Message is a one-line statement of the problem.
	Message string `json:"message"`

	// Detail explains why it matters. It may be several sentences.
	Detail string `json:"detail,omitempty"`

	// Remedy is a single recommended action, written as an imperative.
	Remedy string `json:"remedy,omitempty"`

	// Example is an optional copyable snippet showing the fix. It is always redacted
	// before it reaches this struct.
	Example string `json:"example,omitempty"`
}

// String renders the finding as one human-readable line, without location or remedy.
// The full multi-line rendering belongs to internal/render; this form is for logs and
// test failure messages.
func (f Finding) String() string {
	return fmt.Sprintf("[%s] %s: %s (%s)", f.Severity, f.Target, f.Message, f.Code)
}

// Report is the complete result of a run: every finding plus a summary.
//
// A Report is a value, not a mutable accumulator, so that a renderer cannot mutate the
// evidence it is rendering.
type Report struct {
	// Findings are sorted deterministically. See Sort.
	Findings []Finding `json:"findings"`

	// Checked records how many configuration sources and servers were examined, so a
	// user can tell the difference between "everything passed" and "nothing was found".
	Checked int `json:"checked"`
}

// Add appends a finding to the report.
func (r *Report) Add(f Finding) {
	r.Findings = append(r.Findings, f)
}

// Extend appends every finding in other.
func (r *Report) Extend(other []Finding) {
	r.Findings = append(r.Findings, other...)
}

// Counts returns how many findings exist at each severity.
func (r Report) Counts() (errors, warnings, infos int) {
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityError:
			errors++
		case SeverityWarning:
			warnings++
		case SeverityInfo:
			infos++
		}
	}
	return errors, warnings, infos
}

// Worst returns the highest-severity finding severity present, or SeverityInfo when
// there are no findings.
func (r Report) Worst() Severity {
	worst := SeverityInfo
	for _, f := range r.Findings {
		if f.Severity < worst {
			worst = f.Severity
		}
	}
	return worst
}

// Sort orders findings so that output is byte-for-byte reproducible for a given input.
//
// The order is severity, then code, then target, then location. Go map iteration order
// would otherwise leak into the output and make the tool impossible to diff in CI.
func (r *Report) Sort() {
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Location < b.Location
	})
}

// Exit codes. These are a public contract: pipelines branch on them.
//
// They follow sysexits.h conventions where they overlap, which is the convention most
// CI systems and shell scripts already expect.
const (
	// ExitOK means the run completed and no finding reached the failure threshold.
	ExitOK = 0

	// ExitFindings means the run completed normally and at least one finding reached
	// the failure threshold. This is a diagnostic result, not a tool failure.
	ExitFindings = 1

	// ExitUsage means MCP Doctor could not do its job: bad flags, unreadable input, an
	// internal error. Distinct from ExitFindings so CI can tell a broken config from a
	// broken tool.
	ExitUsage = 2
)

// ExitCodeFor maps a report to a process exit code.
//
// threshold is the lowest severity that counts as failure, controlled by --fail-on. A
// report with findings below the threshold exits OK: a user who wants warnings to fail
// the build asks for it explicitly.
func (r Report) ExitCodeFor(threshold Severity) int {
	for _, f := range r.Findings {
		if f.Severity.atLeast(threshold) {
			return ExitFindings
		}
	}
	return ExitOK
}
