package diagnose

import (
	"testing"
)

func TestSeverityStringIsCanonical(t *testing.T) {
	cases := map[Severity]string{
		SeverityError:   "error",
		SeverityWarning: "warning",
		SeverityInfo:    "info",
		Severity(99):    "unknown",
	}
	for sev, want := range cases {
		if got := sev.String(); got != want {
			t.Errorf("Severity(%d).String() = %q, want %q", int(sev), got, want)
		}
	}
}

func TestSeverityZeroValueIsErrorSoItFailsCI(t *testing.T) {
	// A Finding built without an explicit severity must not silently pass a pipeline.
	var sev Severity
	if sev != SeverityError {
		t.Fatalf("zero Severity = %v, want SeverityError", sev)
	}
}

func TestParseSeverityRoundTrips(t *testing.T) {
	for _, name := range []string{"error", "warning", "info"} {
		sev, err := ParseSeverity(name)
		if err != nil {
			t.Fatalf("ParseSeverity(%q) error: %v", name, err)
		}
		if got := sev.String(); got != name {
			t.Errorf("ParseSeverity(%q).String() = %q, want %q", name, got, name)
		}
	}
}

func TestParseSeverityRejectsUnknownName(t *testing.T) {
	_, err := ParseSeverity("fatal")
	if err == nil {
		t.Fatal("ParseSeverity(\"fatal\") = nil error, want an error")
	}
}

func TestParseSeverityIsCaseAndSpaceInsensitive(t *testing.T) {
	sev, err := ParseSeverity("  WARNING ")
	if err != nil {
		t.Fatalf("ParseSeverity with padding errored: %v", err)
	}
	if sev != SeverityWarning {
		t.Errorf("got %v, want SeverityWarning", sev)
	}
}

func TestExitCodeForUsesThreshold(t *testing.T) {
	tests := []struct {
		name      string
		findings  []Finding
		threshold Severity
		want      int
	}{
		{
			name:      "no findings exits ok at any threshold",
			findings:  nil,
			threshold: SeverityError,
			want:      ExitOK,
		},
		{
			name:      "an error fails an error threshold",
			findings:  []Finding{{Severity: SeverityError}},
			threshold: SeverityError,
			want:      ExitFindings,
		},
		{
			name:      "a warning does not fail an error threshold",
			findings:  []Finding{{Severity: SeverityWarning}},
			threshold: SeverityError,
			want:      ExitOK,
		},
		{
			name:      "a warning fails a warning threshold",
			findings:  []Finding{{Severity: SeverityWarning}},
			threshold: SeverityWarning,
			want:      ExitFindings,
		},
		{
			name:      "an info fails an info threshold",
			findings:  []Finding{{Severity: SeverityInfo}},
			threshold: SeverityInfo,
			want:      ExitFindings,
		},
		{
			name:      "info below warning threshold passes",
			findings:  []Finding{{Severity: SeverityInfo}},
			threshold: SeverityWarning,
			want:      ExitOK,
		},
		{
			name:      "one failing finding among many decides the code",
			findings:  []Finding{{Severity: SeverityInfo}, {Severity: SeverityWarning}, {Severity: SeverityError}},
			threshold: SeverityError,
			want:      ExitFindings,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Report{Findings: tc.findings}
			if got := r.ExitCodeFor(tc.threshold); got != tc.want {
				t.Errorf("ExitCodeFor = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExitFindingsIsDistinctFromUsageFailure(t *testing.T) {
	// A pipeline must be able to tell "your config has problems" from "the tool broke".
	if ExitFindings == ExitUsage {
		t.Fatalf("ExitFindings and ExitUsage are both %d; they must differ", ExitFindings)
	}
}

func TestSortIsDeterministicBySeverityThenCodeThenTarget(t *testing.T) {
	r := Report{Findings: []Finding{
		{Code: "MDC1006", Severity: SeverityInfo, Target: "b"},
		{Code: "MDC1002", Severity: SeverityError, Target: "z"},
		{Code: "MDC1002", Severity: SeverityError, Target: "a"},
		{Code: "MDC1001", Severity: SeverityWarning, Target: "m"},
	}}
	r.Sort()

	want := []struct {
		code string
		sev  Severity
		tgt  string
	}{
		{"MDC1002", SeverityError, "a"},
		{"MDC1002", SeverityError, "z"},
		{"MDC1001", SeverityWarning, "m"},
		{"MDC1006", SeverityInfo, "b"},
	}
	for i, w := range want {
		got := r.Findings[i]
		if got.Code != w.code || got.Severity != w.sev || got.Target != w.tgt {
			t.Errorf("Findings[%d] = {%s %v %s}, want {%s %v %s}",
				i, got.Code, got.Severity, got.Target, w.code, w.sev, w.tgt)
		}
	}
}

func TestSortIsStableAcrossRepeatedRuns(t *testing.T) {
	// Map-derived input is the real source of nondeterminism, so build findings from a
	// map and confirm sorting produces identical output every time.
	build := func() []Finding {
		byTarget := map[string]Severity{"alpha": SeverityInfo, "beta": SeverityError, "gamma": SeverityWarning}
		var out []Finding
		for tgt, sev := range byTarget {
			out = append(out, Finding{Code: "MDC1001", Severity: sev, Target: tgt})
		}
		return out
	}

	first := Report{Findings: build()}
	first.Sort()

	for i := 0; i < 25; i++ {
		next := Report{Findings: build()}
		next.Sort()
		for j := range first.Findings {
			if next.Findings[j].Target != first.Findings[j].Target {
				t.Fatalf("run %d position %d = %q, want %q", i, j, next.Findings[j].Target, first.Findings[j].Target)
			}
		}
	}
}

func TestCountsTalliesEachSeverity(t *testing.T) {
	r := Report{Findings: []Finding{
		{Severity: SeverityError},
		{Severity: SeverityError},
		{Severity: SeverityWarning},
		{Severity: SeverityInfo},
		{Severity: SeverityInfo},
		{Severity: SeverityInfo},
	}}
	errs, warns, infos := r.Counts()
	if errs != 2 || warns != 1 || infos != 3 {
		t.Errorf("Counts() = (%d, %d, %d), want (2, 1, 3)", errs, warns, infos)
	}
}

func TestWorstReportsMostSevereFinding(t *testing.T) {
	if got := (Report{Findings: []Finding{{Severity: SeverityInfo}, {Severity: SeverityWarning}}}).Worst(); got != SeverityWarning {
		t.Errorf("Worst() = %v, want SeverityWarning", got)
	}
	if got := (Report{}).Worst(); got != SeverityInfo {
		t.Errorf("Worst() on empty report = %v, want SeverityInfo", got)
	}
}

func TestAddAndExtendAccumulateFindings(t *testing.T) {
	var r Report
	r.Add(Finding{Code: "MDC0001"})
	r.Extend([]Finding{{Code: "MDC0002"}, {Code: "MDC0003"}})
	if len(r.Findings) != 3 {
		t.Fatalf("len(Findings) = %d, want 3", len(r.Findings))
	}
}
