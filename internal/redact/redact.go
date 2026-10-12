// Package redact removes sensitive values from anything MCP Doctor prints.
//
// It is a leaf package with no dependencies on the rest of the tool, on purpose: secret
// redaction is only trustworthy if every output path is forced through it, and a leaf
// with no call graph of its own is the cheapest way to guarantee that.
//
// The model is deliberately simple. A caller registers the values it knows are secret
// (environment values, header values), and every string that will be shown to a human or
// written to a file is passed through [Redactor.Redact] before it leaves the process.
// Pattern matching is layered on top to catch secrets the tool never saw directly, such
// as a token a server echoes to stderr.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Placeholder is what replaces a redacted value. It is intentionally fixed so that
// output is deterministic and a test can assert on it.
const Placeholder = "***REDACTED***"

// minSecretLen is the shortest string that will be registered as a secret.
//
// Without a floor, registering env values like "1" or "true" would redact those two
// characters everywhere, turning the whole report into noise. Short values are handled
// by the pattern matchers instead, which are precise about what they match.
const minSecretLen = 4

// commonEnvAllowlist are env var names whose values are never treated as secret.
//
// These are conventional, non-sensitive, and appear constantly in MCP configs (PATH
// rules, locale settings). Treating them as secrets would redact ordinary output and
// teach users to ignore redaction entirely, which is worse than not redacting at all.
var commonEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TZ": true,
	"TMPDIR": true, "PWD": true, "OLDPWD": true, "EDITOR": true, "PAGER": true,
	"HOME_DIR": true, "APPDATA": true, "LOCALAPPDATA": true, "PROGRAMFILES": true,
	"OS": true, "ARCH": true, "NODE_ENV": true, "PYTHONPATH": true, "VIRTUAL_ENV": true,
	"GOPATH": true, "GOROOT": true, "XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true,
	"XDG_DATA_HOME": true, "COLORTERM": true, "DISPLAY": true, "HOSTNAME": true,
	"COMSPEC": true, "PATHEXT": true, "SYSTEMROOT": true, "WINDIR": true, "NUMBER_OF_PROCESSORS": true,
}

// secretPatterns match credential shapes the tool never saw as a config value.
//
// Each pattern targets a shape that is overwhelmingly a credential in practice. A false
// positive costs a redacted word in a diagnostic; a false negative costs a leaked key,
// so the patterns lean toward matching.
var secretPatterns = []*regexp.Regexp{
	// Provider-issued keys with a recognisable prefix. The trailing class is bounded so
	// that ordinary prose containing "sk-" is not swallowed.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{20,}\b`),

	// JSON Web Tokens.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),

	// Basic-auth credentials embedded in a URL.
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`),

	// key=value / "key": "value" forms for names that are secrets by convention. The
	// variable name must stay INSIDE the capture group: the user needs to know which
	// variable leaked, and replacing the whole match would hide that.
	regexp.MustCompile(`(?i)\b((?:api[_-]?key|apikey|secret|password|passwd|token|access[_-]?token|refresh[_-]?token|client[_-]?secret|private[_-]?key|auth[_-]?token|session[_-]?key|bearer)["']?\s*[:=]\s*)["']?[^\s"',}]{4,}["']?`),

	// PEM private key blocks, including their BEGIN line.
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),

	// Shell-style export of a secret-looking variable.
	regexp.MustCompile(`(?i)\bexport\s+((?:API_KEY|APIKEY|SECRET|PASSWORD|TOKEN|ACCESS_TOKEN|PRIVATE_KEY|AUTH_TOKEN|SESSION_KEY))\s*=\s*\S+`),
}

// Redactor scrubs secrets from strings.
//
// A Redactor is safe for concurrent use. A single instance is normally shared across a
// run so that every output path scrubs against the same set of known secrets.
type Redactor struct {
	mu      sync.RWMutex
	secrets map[string]struct{}
}

// New returns a Redactor with no registered secrets. The result is ready to use and
// non-nil, so callers never have to guard against a nil Redactor.
func New() *Redactor {
	return &Redactor{secrets: make(map[string]struct{})}
}

// Add registers a literal value as secret. Short values are ignored unless force is set,
// because redacting a two-character value corrupts unrelated output.
//
// Add is how config env and header values reach the redactor: the tool learns the secret
// at the moment it reads it, and everything printed afterwards is scrubbed against it.
func (r *Redactor) Add(value string, force bool) {
	if r == nil {
		return
	}
	if value == "" || (!force && len(value) < minSecretLen) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets[value] = struct{}{}
}

// AddEnv registers the value of an environment variable as secret, unless the variable's
// name is a well-known non-sensitive one.
//
// The allowlist is what keeps ordinary values usable: PATH and LANG are not secrets, and
// redacting them would make every diagnostic mentioning a path unreadable.
func (r *Redactor) AddEnv(name, value string) {
	if commonEnvAllowlist[name] {
		return
	}
	r.Add(value, false)
}

// Redact returns s with every known secret and every pattern-matched credential
// replaced by [Placeholder].
//
// It is the single sink every user-visible string must pass through. Redaction is
// applied to the whole string, so a secret split across a line boundary is still caught.
func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	out := r.redactKnown(s)
	out = redactPatterns(out)
	return out
}

// RedactArgs returns a copy of args with every element redacted, which is how a command
// line containing an inline secret is reported without exposing it.
func (r *Redactor) RedactArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = r.Redact(a)
	}
	return out
}

// RedactMap returns a copy of m with both keys and values redacted, for reporting an
// environment or header map without disclosing its contents.
func (r *Redactor) RedactMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[r.Redact(k)] = r.Redact(v)
	}
	return out
}

// redactKnown replaces every registered literal occurrence, longest first.
//
// Longest-first matters: if both "abc" and "abcdef" are registered, replacing the short
// one first would leave "def" exposed where the long one matched.
func (r *Redactor) redactKnown(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	if len(r.secrets) == 0 {
		r.mu.RUnlock()
		return s
	}
	// Copy under the read lock so the sort below runs without holding it, which would
	// otherwise block every concurrent Add for the duration of the sort.
	secrets := make([]string, 0, len(r.secrets))
	for sec := range r.secrets {
		secrets = append(secrets, sec)
	}
	r.mu.RUnlock()

	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) != len(secrets[j]) {
			return len(secrets[i]) > len(secrets[j])
		}
		return secrets[i] < secrets[j]
	})

	for _, sec := range secrets {
		if sec == "" || !strings.Contains(s, sec) {
			continue
		}
		s = strings.ReplaceAll(s, sec, Placeholder)
	}
	return s
}

// redactPatterns applies every credential pattern to s.
func redactPatterns(s string) string {
	for _, re := range secretPatterns {
		if !re.MatchString(s) {
			continue
		}
		switch {
		case re.String() == `([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`:
			// Keep the scheme so the URL is still readable, drop the credentials.
			s = re.ReplaceAllString(s, "${1}"+Placeholder+"@")
		case strings.HasPrefix(re.String(), `(?i)\bexport\s+`):
			s = re.ReplaceAllString(s, "export ${1}="+Placeholder)
		case strings.HasPrefix(re.String(), `(?i)\b((?:api[_-]?key`):
			// Preserve the key name and separator so the diagnostic still says which
			// variable leaked, without disclosing the value.
			s = re.ReplaceAllString(s, "${1}"+Placeholder)
		default:
			s = re.ReplaceAllString(s, Placeholder)
		}
	}
	return s
}

// Fingerprint returns a short, stable, non-reversible identifier for a value.
//
// It exists so two occurrences of the same secret can be correlated in a report ("the
// same key appeared in both the env block and the stderr excerpt") without either
// occurrence revealing the secret. A truncated SHA-256 is not a secret and not reversible
// in practice.
func Fingerprint(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// LooksSecret reports whether a config value has the shape of a credential.
//
// It is how the tool warns that a key was committed to a repository. It reports a
// boolean and never the value, because the caller's purpose is to decide whether to
// tell the user something, not to print the thing itself.
func LooksSecret(name, value string) bool {
	if value == "" {
		return false
	}
	lowerName := strings.ToLower(name)
	for _, hint := range []string{"key", "secret", "token", "password", "passwd", "credential", "auth"} {
		if strings.Contains(lowerName, hint) {
			return true
		}
	}
	for _, re := range secretPatterns {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

// StripControlChars removes ASCII control characters and ANSI escape sequences from s.
//
// This defends the terminal, not just the report. A hostile MCP server controls its own
// stdout and stderr; without stripping, it could repaint the screen or forge a
// diagnostic line that looks like MCP Doctor's own output. Newlines and tabs are
// preserved because they carry meaning in multi-line diagnostics.
func StripControlChars(s string) string {
	if s == "" {
		return s
	}
	s = ansiEscape.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// Drop the control byte entirely.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ansiEscape matches CSI and OSC escape sequences.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// Truncate bounds s to at most max bytes, appending an explicit marker when it had to
// cut. The marker matters: a silently truncated secret excerpt would read as if the
// output ended there, which is misleading during a diagnosis.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	const marker = "… [truncated]"
	if max <= len(marker) {
		return s[:max]
	}
	return s[:max-len(marker)] + marker
}

// Sanitize is the convenience combination every rendering path should use: strip
// control characters, redact secrets, then bound the length.
//
// The order matters. Stripping first means an attacker cannot hide a secret behind a
// control character; redacting second means the length bound applies to already-safe
// text.
func (r *Redactor) Sanitize(s string, max int) string {
	return Truncate(r.Redact(StripControlChars(s)), max)
}
