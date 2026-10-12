package diagnose

import (
	"regexp"
	"strings"
	"testing"
)

// Diagnostic codes are a public contract that users automate against. This test guards
// the invariants that make that contract usable, rather than checking any single code.
var codePattern = regexp.MustCompile(`^MDC[0-9]{4}$`)

func collectCodes() map[string]bool {
	// Reflection-free by design: the constants are compile-time values, so this test
	// enumerates them through the tables below, which must stay in sync with codes.go.
	all := allCodes()
	return func() map[string]bool {
		m := make(map[string]bool, len(all))
		for _, c := range all {
			m[c] = true
		}
		return m
	}()
}

// allCodes returns every declared code in a stable order.
func allCodes() []string {
	return append(append([]string{}, configCodes()...), append(serverCodes(), transportCodes()...)...)
}

func TestEveryCodeMatchesThePublicShape(t *testing.T) {
	for _, c := range allCodes() {
		if !codePattern.MatchString(c) {
			t.Errorf("code %q does not match %s", c, codePattern)
		}
	}
}

func TestNoCodeIsDeclaredTwice(t *testing.T) {
	// Reusing a code for a different meaning silently breaks every user who matched
	// on it, which is the failure mode this test exists to prevent.
	all := allCodes()
	seen := make(map[string]int, len(all))
	for i, c := range all {
		seen[c]++
		if seen[c] == 2 {
			// Report both positions so the duplicate is easy to locate.
			var first int
			for j := 0; j < i; j++ {
				if all[j] == c {
					first = j
					break
				}
			}
			t.Errorf("code %q declared at positions %d and %d", c, first, i)
		}
	}
}

func TestCodeRangesAreReservedPerConcern(t *testing.T) {
	// Range membership is a readability convention, so drift here would mislead anyone
	// scanning the docs by range.
	groups := map[string][]string{
		"config access": configCodes(),
		"static config": serverCodes(),
		"protocol":      transportCodes(),
	}
	wantPrefix := map[string]string{
		"config access": "MDC0",
		"static config": "MDC1",
		"protocol":      "MDC2",
	}
	for name, codes := range groups {
		for _, c := range codes {
			if !strings.HasPrefix(c, wantPrefix[name]) {
				t.Errorf("%s code %q does not start with %q", name, c, wantPrefix[name])
			}
		}
	}
}

// TestToolCodesAreDefined guards the codes referenced by the CLI itself. A typo'd code
// would otherwise ship silently, because nothing else references these constants.
func TestToolCodesAreDefined(t *testing.T) {
	// These are the codes the CLI itself can emit. They must be real codes so that a
	// user's match on the documented set covers them.
	declared := collectCodes()
	used := []string{
		CodeProbeNotAuthorized,
		CodeRemoteNotAllowed,
		CodeConfigUnreadable,
	}
	for _, c := range used {
		if !declared[c] {
			t.Errorf("code %q used by the CLI is not in the declared set", c)
		}
	}
}
