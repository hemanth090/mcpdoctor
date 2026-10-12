package protocol

import (
	"io"
	"net/url"
	"os"
	"strings"
)

// Kind identifies the transport a target uses.
type Kind string

const (
	// KindStdio launches a subprocess and speaks over its stdin and stdout.
	KindStdio Kind = "stdio"

	// KindHTTP speaks Streamable HTTP to a remote endpoint.
	KindHTTP Kind = "http"
)

// Target is one server to probe, resolved from a configuration entry.
//
// It is a plain value with no reference back to the config package, so this package
// stays independent of how configurations are discovered and parsed.
type Target struct {
	// Name is the server's configured name.
	Name string

	// Kind is the transport to use.
	Kind Kind

	// Command and Args are the executable and arguments for a stdio server.
	Command string
	Args    []string

	// Cwd is the working directory for a stdio server.
	Cwd string

	// Env is the environment the configuration requested. It is merged over the
	// allowlisted host environment at launch time.
	Env map[string]string

	// URL is the endpoint for an HTTP server.
	URL string

	// Headers are the configured HTTP headers.
	Headers map[string]string
}

// targetEnvAllowlist is the set of host variables forwarded to a stdio child.
//
// The list is short on purpose. A server is third-party code, so the default position is
// to share nothing, and every variable here is one a server genuinely cannot run
// without. Everything else, including every credential the user holds, stays with us.
var targetEnvAllowlist = []string{
	"PATH",
	"HOME",
	"TMPDIR",
	"TEMP", "TMP",
	"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH",
	"LANG", "LC_ALL", "TZ",
	"NODE_PATH", "PYTHONPATH", "VIRTUAL_ENV", "GOPATH", "GOROOT",
}

// lookupEnv reads an environment variable.
//
// It is a package-level function rather than a direct os.Getenv call at each site so
// that tests can substitute a controlled environment instead of depending on whatever
// the CI runner exports.
var lookupEnv = os.Getenv

// hostEnvAllowlist returns the host values that may be forwarded to a child.
//
// The whole host environment is never passed through: doing so would hand a third-party
// server every credential the user's shell holds. Only the variables on the allowlist
// are read, and the child is given exactly that set.
func hostEnvAllowlist() map[string]string {
	out := make(map[string]string, len(targetEnvAllowlist))
	for _, name := range targetEnvAllowlist {
		if v := lookupEnv(name); v != "" {
			out[name] = v
		}
	}
	return out
}

// buildTransportEnv merges the allowlisted host environment with the configuration's
// requested variables.
//
// Configuration values win, because the user wrote them into the config specifically for
// this server. The result is passed to the child as its complete environment.
func buildTransportEnv(configured map[string]string) map[string]string {
	env := hostEnvAllowlist()
	for k, v := range configured {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			// A name containing "=" or NUL cannot be a valid variable name and would
			// corrupt the environment block. Skip it rather than emitting something the
			// child cannot parse.
			continue
		}
		env[k] = v
	}
	return env
}

// validateTargetURL checks that a URL is usable as an MCP endpoint.
//
// It is a defensive re-check. The configuration validator already reported these
// conditions statically, but a probe may be invoked directly on a target that never went
// through validation, and connecting to an unexpected scheme would be worse than
// refusing.
func validateTargetURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errUnsupportedScheme
	}
	if u.Host == "" {
		return errNoHost
	}
	if strings.ContainsRune(raw, '\x00') {
		return errNULInURL
	}
	return nil
}

// Sentinel errors for URL validation, so callers can classify with errors.Is rather than
// by matching on message text.
var (
	errUnsupportedScheme = &urlError{"unsupported URL scheme"}
	errNoHost            = &urlError{"URL has no host"}
	errNULInURL          = &urlError{"URL contains a NUL byte"}
)

// urlError is a simple typed error for endpoint validation failures.
type urlError struct{ msg string }

func (e *urlError) Error() string { return e.msg }

// stderrLimit is the bound on captured server stderr.
const stderrLimit = 64 << 10

// nopWriteCloser adapts an io.WriteCloser for callers that only need an io.Writer.
//
// It exists because the SDK's IOTransport takes an io.Writer while our process gives us
// an io.WriteCloser, and closing is handled by the process lifecycle rather than by the
// transport.
type nopWriteCloser struct{ w io.Writer }

func (n nopWriteCloser) Write(p []byte) (int, error) { return n.w.Write(p) }

// ensure the io import is meaningfully used even if the adapter is currently unused in
// a future refactor.
var _ = nopWriteCloser{}
