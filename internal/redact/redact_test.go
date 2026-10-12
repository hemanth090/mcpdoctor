package redact

import (
	"strings"
	"testing"
)

// syntheticSecret is a fake credential used only in tests. It must never look like a
// real provider key, and it must never be committed to a fixture as a live value.
const syntheticSecret = "test-SECRET-abc123xyz789"

func TestRedactReplacesRegisteredLiteralSecret(t *testing.T) {
	r := New()
	r.Add(syntheticSecret, false)

	got := r.Redact("connecting with key " + syntheticSecret + " to api")
	if strings.Contains(got, syntheticSecret) {
		t.Fatalf("Redact left the secret in output: %q", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Errorf("Redact output missing placeholder: %q", got)
	}
	// Surrounding context must survive, or the diagnostic becomes unreadable.
	if !strings.Contains(got, "connecting with key") {
		t.Errorf("Redact destroyed surrounding context: %q", got)
	}
}

func TestRedactIgnoresShortValuesSoOutputStaysReadable(t *testing.T) {
	// Without a length floor, registering env "1" or "true" would shred every message.
	r := New()
	r.Add("1", false)
	r.Add("ab", false)

	in := "server returned 1 byte and status ab"
	if got := r.Redact(in); got != in {
		t.Errorf("Redact modified short values: got %q, want %q", got, in)
	}
}

func TestAddForceRegistersShortValues(t *testing.T) {
	// force exists for the case where the user explicitly told us a short string is a
	// credential, which beats our length heuristic.
	r := New()
	r.Add("pw7", true)

	if got := r.Redact("pw7"); got != Placeholder {
		t.Errorf("Redact(%q) = %q, want %q", "pw7", got, Placeholder)
	}
}

func TestAddEnvSkipsWellKnownNonSensitiveVariables(t *testing.T) {
	// PATH and friends are not secrets; redacting them would make paths unreadable.
	r := New()
	r.AddEnv("PATH", "/usr/local/bin:/usr/bin")
	r.AddEnv("HOME", "/home/alice")
	r.AddEnv("LANG", "en_US.UTF-8")

	in := "spawning with PATH=/usr/local/bin:/usr/bin and HOME=/home/alice"
	if got := r.Redact(in); got != in {
		t.Errorf("AddEnv redacted a non-sensitive variable:\n got %q\nwant %q", got, in)
	}
}

func TestAddEnvRegistersSensitiveVariables(t *testing.T) {
	r := New()
	r.AddEnv("GITHUB_TOKEN", syntheticSecret)

	if got := r.Redact("token is " + syntheticSecret); strings.Contains(got, syntheticSecret) {
		t.Fatalf("AddEnv failed to register a sensitive value: %q", got)
	}
}

func TestAddEnvEmptyValueIsHarmless(t *testing.T) {
	r := New()
	r.AddEnv("SOME_VAR", "")
	if got := r.Redact("nothing to redact here"); got != "nothing to redact here" {
		t.Errorf("Redact = %q, want unchanged", got)
	}
}

func TestRedactHandlesOverlappingSecretsLongestFirst(t *testing.T) {
	// If the short secret were replaced first, the long one's tail would survive.
	r := New()
	r.Add("SECRET", false)
	r.Add("SECRET-AND-MORE-STUFF", false)

	in := "key=SECRET-AND-MORE-STUFF end"
	got := r.Redact(in)
	if strings.Contains(got, "AND-MORE-STUFF") {
		t.Errorf("longer secret not fully redacted: %q", got)
	}
	if strings.Contains(got, "SECRET") {
		t.Errorf("secret substring survived: %q", got)
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	r := New()
	r.Add(syntheticSecret, false)

	once := r.Redact("key " + syntheticSecret)
	twice := r.Redact(once)
	if once != twice {
		t.Errorf("Redact not idempotent:\n once %q\ntwice %q", once, twice)
	}
}

func TestRedactMatchesCredentialPatternsItNeverSaw(t *testing.T) {
	// A server can echo a credential it fetched itself. We never registered that value,
	// so only the pattern layer can catch it.
	r := New()
	in := "server said: token REDACTED_SAMPLE_TOKEN done"

	got := r.Redact(in)
	if strings.Contains(got, "eyJ_REDACTED_SAMPLE_HEADER") {
		t.Errorf("JWT survived redaction: %q", got)
	}
	if !strings.Contains(got, "server said: token") {
		t.Errorf("pattern redaction destroyed context: %q", got)
	}
}

func TestRedactCatchesProviderKeyPrefixes(t *testing.T) {
	cases := map[string]string{
		"openai-style": "sk_REDACTED_SAMPLE",
		"github":       "ghp_REDACTED_SAMPLE",
		"aws":          "AKIA_REDACTED_SAMPLE",
		"slack":        "xoxb_REDACTED_SAMPLE",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			r := New()
			got := r.Redact("using " + key + " to authenticate")
			if strings.Contains(got, key) {
				t.Errorf("key survived redaction: %q", got)
			}
		})
	}
}

func TestRedactPreservesSchemeWhenStrippingURLCredentials(t *testing.T) {
	// The scheme is not sensitive and makes the diagnostic readable, so it stays.
	r := New()
	got := r.Redact("connecting to https://admin:hunter2@example.com/mcp")
	if strings.Contains(got, "hunter2") {
		t.Errorf("URL password survived: %q", got)
	}
	if !strings.Contains(got, "https://") {
		t.Errorf("scheme was destroyed: %q", got)
	}
	if !strings.Contains(got, "example.com/mcp") {
		t.Errorf("host and path were destroyed: %q", got)
	}
}

func TestRedactPreservesKeyNameForKeyValuePairs(t *testing.T) {
	// The user needs to know *which* variable leaked; they do not need its value.
	r := New()
	got := r.Redact(`{"api_key": "supersecretvalue123"}`)
	if strings.Contains(got, "supersecretvalue123") {
		t.Errorf("value survived: %q", got)
	}
	if !strings.Contains(got, "api_key") {
		t.Errorf("key name was destroyed: %q", got)
	}
}

func TestRedactRemovesPrivateKeyBlocks(t *testing.T) {
	r := New()
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIE_REDACTED_SAMPLE\nsecretline\n-----END RSA PRIVATE KEY-----"
	got := r.Redact("config contained " + pem)
	if strings.Contains(got, "secretline") {
		t.Errorf("private key body survived: %q", got)
	}
}

func TestStripControlCharsRemovesAnsiEscapes(t *testing.T) {
	// A hostile server controls its stderr. Without stripping it could repaint the
	// terminal or forge a line that looks like our own diagnostic output.
	in := "\x1b[31mERROR\x1b[0m: something failed"
	got := StripControlChars(in)
	if strings.Contains(got, "\x1b") {
		t.Errorf("ANSI escape survived: %q", got)
	}
	if !strings.Contains(got, "ERROR") {
		t.Errorf("text was destroyed: %q", got)
	}
	if !strings.Contains(got, "something failed") {
		t.Errorf("message tail was destroyed: %q", got)
	}
}

func TestStripControlCharsRemovesBellAndOtherControls(t *testing.T) {
	in := "line one\x07 line two\x00 end"
	got := StripControlChars(in)
	if strings.ContainsAny(got, "\x07\x00") {
		t.Errorf("control bytes survived: %q", got)
	}
}

func TestStripControlCharsPreservesNewlinesAndTabs(t *testing.T) {
	// Multi-line stderr excerpts are the most useful diagnostic we have; collapsing
	// their whitespace would destroy the evidence.
	in := "first line\n\tindented line"
	if got := StripControlChars(in); got != in {
		t.Errorf("StripControlChars = %q, want %q", got, in)
	}
}

func TestTruncateBoundsLengthAndMarksTheCut(t *testing.T) {
	in := strings.Repeat("a", 100)
	got := Truncate(in, 20)
	if len(got) > 20 {
		t.Errorf("len = %d, want <= 20", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncation was not marked: %q", got)
	}
}

func TestTruncateLeavesShortStringsAlone(t *testing.T) {
	if got := Truncate("short", 100); got != "short" {
		t.Errorf("Truncate modified a short string: %q", got)
	}
}

func TestTruncateHandlesNonPositiveMax(t *testing.T) {
	if got := Truncate("anything", 0); got != "" {
		t.Errorf("Truncate(_, 0) = %q, want empty", got)
	}
}

func TestSanitizeStripsThenRedactsThenBounds(t *testing.T) {
	r := New()
	r.Add(syntheticSecret, false)

	in := "\x1b[31mleaked " + syntheticSecret + "\x00" + strings.Repeat("x", 500)
	got := r.Sanitize(in, 64)

	if strings.Contains(got, syntheticSecret) {
		t.Errorf("Sanitize leaked the secret: %q", got)
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\x00") {
		t.Errorf("Sanitize left control characters: %q", got)
	}
	if len(got) > 64 {
		t.Errorf("len = %d, want <= 64", len(got))
	}
}

func TestSanitizeDoesNotLetControlCharactersHideASecret(t *testing.T) {
	// If stripping ran after redaction, an attacker could interleave a control byte to
	// break the match and get a secret through.
	r := New()
	r.Add("SECRETVALUE", false)

	in := "SECRET\x07VALUE"
	got := r.Sanitize(in, 200)
	if strings.Contains(got, "SECRET") && strings.Contains(got, "VALUE") {
		t.Errorf("secret survived because a control char split it: %q", got)
	}
}

func TestFingerprintIsStableDistinctAndNonReversible(t *testing.T) {
	a := Fingerprint("value-one")
	b := Fingerprint("value-two")

	if a == "" {
		t.Fatal("Fingerprint returned empty")
	}
	if a != Fingerprint("value-one") {
		t.Error("Fingerprint is not stable for the same input")
	}
	if a == b {
		t.Error("Fingerprint collided for different inputs")
	}
	if strings.Contains(a, "value-one") {
		t.Errorf("Fingerprint leaks the input: %q", a)
	}
}

func TestFingerprintOfEmptyIsEmpty(t *testing.T) {
	if got := Fingerprint(""); got != "" {
		t.Errorf("Fingerprint(\"\") = %q, want empty", got)
	}
}

func TestLooksSecretDetectsByName(t *testing.T) {
	for _, name := range []string{"API_KEY", "my_secret", "auth_token", "DB_PASSWORD", "client_secret"} {
		if !LooksSecret(name, "someordinaryvalue") {
			t.Errorf("LooksSecret(%q, ...) = false, want true", name)
		}
	}
}

func TestLooksSecretDetectsByShape(t *testing.T) {
	if !LooksSecret("SOME_VAR", "sk_REDACTED_SAMPLE") {
		t.Error("LooksSecret did not detect a key by shape")
	}
}

func TestLooksSecretIgnoresOrdinaryValues(t *testing.T) {
	if LooksSecret("LOG_LEVEL", "debug") {
		t.Error("LooksSecret flagged an ordinary value")
	}
	if LooksSecret("EMPTY", "") {
		t.Error("LooksSecret flagged an empty value")
	}
}

func TestNilRedactorIsUsableSoCallersNeedNoNilGuards(t *testing.T) {
	// A nil *Redactor used through these methods must not panic. Every call site would
	// otherwise need a guard, and a forgotten guard is a secret leak.
	var r *Redactor
	r.Add("something", false)
	r.AddEnv("TOKEN", syntheticSecret)

	if got := r.Redact("plain text"); got != "plain text" {
		t.Errorf("nil Redactor changed output: %q", got)
	}
	if got := r.RedactArgs([]string{"a"}); len(got) != 1 {
		t.Errorf("nil Redactor RedactArgs = %v", got)
	}
	if got := r.RedactMap(map[string]string{"a": "b"}); got["a"] != "b" {
		t.Errorf("nil Redactor RedactMap = %v", got)
	}
}

func TestRedactArgsRedactsEachElement(t *testing.T) {
	r := New()
	r.Add(syntheticSecret, false)

	got := r.RedactArgs([]string{"--token", syntheticSecret, "--verbose"})
	if strings.Contains(strings.Join(got, " "), syntheticSecret) {
		t.Errorf("secret survived in args: %v", got)
	}
	if got[0] != "--token" || got[2] != "--verbose" {
		t.Errorf("non-secret args were modified: %v", got)
	}
}

func TestRedactArgsHandlesNil(t *testing.T) {
	if got := New().RedactArgs(nil); got != nil {
		t.Errorf("RedactArgs(nil) = %v, want nil", got)
	}
}

func TestRedactMapRedactsKeysAndValues(t *testing.T) {
	r := New()
	r.Add(syntheticSecret, false)

	got := r.RedactMap(map[string]string{"TOKEN": syntheticSecret})
	if got["TOKEN"] == syntheticSecret {
		t.Errorf("value survived in map: %v", got)
	}
}

func TestRedactMapHandlesNil(t *testing.T) {
	if got := New().RedactMap(nil); got != nil {
		t.Errorf("RedactMap(nil) = %v, want nil", got)
	}
}

func TestRedactorIsSafeForConcurrentUse(t *testing.T) {
	// Renderers and the probe both register and redact; a data race here would be a
	// correctness bug, not just a flaky test. Run with -race to make this meaningful.
	r := New()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.AddEnv("TOKEN", syntheticSecret)
		}
	}()
	for i := 0; i < 200; i++ {
		_ = r.Redact("key " + syntheticSecret)
	}
	<-done
}
