package protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mcpdoctor/mcpdoctor/internal/diagnose"
	"github.com/mcpdoctor/mcpdoctor/internal/redact"
	"github.com/mcpdoctor/mcpdoctor/internal/testutil"
)

// probeEndpoint runs a probe against a Streamable HTTP endpoint.
func probeEndpoint(t *testing.T, url string, opts Options) Result {
	t.Helper()
	opts.Execute = true
	res, err := Probe(context.Background(), Target{Name: "http-server", Kind: KindHTTP, URL: url}, opts, redact.New())
	if err != nil {
		t.Fatalf("Probe returned an error: %v", err)
	}
	return res
}

func TestHTTPProbeCompletesHandshakeAgainstARealServer(t *testing.T) {
	srv := testutil.HTTPServer(t, testutil.Behaviour{
		ServerName:     "http-demo",
		ServerVersion:  "3.0.0",
		AdvertiseTools: true,
		Tools:          []testutil.Tool{{Name: "alpha"}, {Name: "beta"}},
	})

	res := probeEndpoint(t, srv.URL, Options{Timeout: 20 * time.Second})

	for _, f := range res.Findings {
		if f.Severity == diagnose.SeverityError {
			t.Errorf("healthy HTTP server produced an error finding [%s] %s: %s", f.Code, f.Message, f.Detail)
		}
	}
	if res.ServerName != "http-demo" {
		t.Errorf("server name = %q, want http-demo", res.ServerName)
	}
	if res.NegotiatedVersion == "" {
		t.Error("no protocol version negotiated over HTTP")
	}
	if res.ToolCount != 2 {
		t.Errorf("tool count = %d, want 2", res.ToolCount)
	}
}

func TestHTTPProbeRefusesARemoteHostWithoutAllowRemote(t *testing.T) {
	// A public host is the common accidental case: someone pasted a URL from an example
	// and the tool must not start making requests to it.
	res := probeEndpoint(t, "http://example.com/mcp", Options{Timeout: 5 * time.Second})

	if !hasCode(res.Findings, diagnose.CodeRemoteNotAllowed) {
		t.Fatalf("expected CodeRemoteNotAllowed, got %v", codesOf(res.Findings))
	}
	if res.NegotiatedVersion != "" {
		t.Error("a remote host was contacted without authorisation")
	}
}

func TestHTTPProbeAllowsLoopbackWithoutAllowRemote(t *testing.T) {
	srv := testutil.HTTPServer(t, testutil.Behaviour{
		ServerName:     "local",
		AdvertiseTools: true,
		Tools:          []testutil.Tool{{Name: "x"}},
	})
	res := probeEndpoint(t, srv.URL, Options{Timeout: 20 * time.Second})

	if hasCode(res.Findings, diagnose.CodeRemoteNotAllowed) {
		t.Error("a loopback endpoint should not require --allow-remote")
	}
	if res.ToolCount != 1 {
		t.Errorf("tool count = %d, want 1", res.ToolCount)
	}
}

func TestHTTPProbeReportsAnUnreachableEndpoint(t *testing.T) {
	// A closed port must produce a clear transport diagnosis, not a hang.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	res := probeEndpoint(t, url, Options{Timeout: 10 * time.Second})

	if len(res.Findings) == 0 {
		t.Fatal("an unreachable endpoint produced no findings")
	}
	if res.NegotiatedVersion != "" {
		t.Error("a version was negotiated against a dead endpoint")
	}
}

func TestHTTPProbeReportsANonMCPEndpoint(t *testing.T) {
	// A server that answers but is not an MCP endpoint is a common misconfiguration:
	// the URL points at the wrong service.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not an MCP server</html>"))
	}))
	defer srv.Close()

	res := probeEndpoint(t, srv.URL, Options{Timeout: 10 * time.Second})

	if len(res.Findings) == 0 {
		t.Fatal("a non-MCP endpoint produced no findings")
	}
	if res.NegotiatedVersion != "" {
		t.Error("a protocol version was negotiated with a non-MCP endpoint")
	}
}

func TestHTTPProbeIsBoundedWhenTheEndpointHangs(t *testing.T) {
	// A server that accepts the connection and never answers must not hang the tool.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer srv.Close()

	start := time.Now()
	res := probeEndpoint(t, srv.URL, Options{Timeout: 2 * time.Second})
	elapsed := time.Since(start)

	if elapsed > 15*time.Second {
		t.Errorf("probe took %v against a hanging endpoint, want it bounded near 2s", elapsed)
	}
	if len(res.Findings) == 0 {
		t.Error("a hanging endpoint produced no findings")
	}
}

func TestHTTPProbeReportsAnOversizedResponse(t *testing.T) {
	// An endpoint that streams an enormous body must not be allowed to exhaust memory.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		chunk := strings.Repeat("A", 64<<10)
		for i := 0; i < 200; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	res := probeEndpoint(t, srv.URL, Options{Timeout: 15 * time.Second})
	// The point is that we survive it and say something; the specific code depends on
	// where the transport gives up.
	if len(res.Findings) == 0 {
		t.Error("an oversized response produced no findings")
	}
}

func TestHTTPProbeFollowsTheMCPProtocolVersionHeader(t *testing.T) {
	// The specification requires MCP-Protocol-Version on every request after
	// initialization. A server that never sees it is a server we cannot fully validate,
	// so the probe should still succeed; this test guards that a conforming server does
	// not break the probe.
	srv := testutil.HTTPServer(t, testutil.Behaviour{
		AdvertiseTools: true,
		Tools:          []testutil.Tool{{Name: "t"}},
	})
	res := probeEndpoint(t, srv.URL, Options{Timeout: 20 * time.Second})

	if res.NegotiatedVersion == "" {
		t.Fatalf("handshake did not complete:\n%+v", res.Findings)
	}
}

func TestBuildHTTPClientHasExplicitTimeouts(t *testing.T) {
	// Every one of these limits is a control. Removing them silently would reintroduce
	// the unbounded-read and hanging-connection problems the threat model names.
	c := buildHTTPClient("https://example.com/mcp", Options{RequestTimeout: 3 * time.Second})

	if c.Timeout == 0 {
		t.Error("client has no overall timeout")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("no response header timeout")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("no TLS handshake timeout")
	}
	if tr.MaxResponseHeaderBytes == 0 {
		t.Error("response headers are unbounded")
	}
}

func TestBuildHTTPClientBoundsRedirects(t *testing.T) {
	c := buildHTTPClient("https://example.com/mcp", Options{})
	if c.CheckRedirect == nil {
		t.Fatal("no redirect policy; a redirect chain could reach an internal service")
	}

	// Build a chain and confirm it is refused at the limit.
	req, _ := http.NewRequest("GET", "https://example.com/1", nil)
	via := make([]*http.Request, 0, 4)
	for i := 0; i < 3; i++ {
		via = append(via, req)
	}
	if err := c.CheckRedirect(req, via); err == nil {
		t.Error("a redirect chain of 3 was allowed, want refusal")
	}
}

func TestBuildHTTPClientRefusesHTTPDowngradeRedirect(t *testing.T) {
	// A redirect from https to plain http would silently downgrade the connection and
	// expose any credentials in the request.
	c := buildHTTPClient("https://example.com/mcp", Options{})
	req, _ := http.NewRequest("GET", "http://evil.example.com/mcp", nil)
	via := []*http.Request{{}}

	if err := c.CheckRedirect(req, via); err == nil {
		t.Error("redirect to plain http was allowed, want refusal")
	}
}

func TestBuildHTTPClientAllowsLoopbackHTTPRedirect(t *testing.T) {
	c := buildHTTPClient("http://127.0.0.1/mcp", Options{})
	req, _ := http.NewRequest("GET", "http://127.0.0.1:9000/mcp", nil)
	via := []*http.Request{{}}

	if err := c.CheckRedirect(req, via); err != nil {
		t.Errorf("loopback redirect was refused: %v", err)
	}
}

func TestBuildTransportEnvForwardsOnlyTheAllowlist(t *testing.T) {
	// The allowlist is the control that stops a server inheriting every credential in
	// the user's shell.
	original := lookupEnv
	lookupEnv = func(name string) string {
		switch name {
		case "PATH":
			return "/usr/bin"
		case "HOME":
			return "/home/tester"
		case "AWS_SECRET_ACCESS_KEY":
			return "super-secret-value"
		default:
			return ""
		}
	}
	defer func() { lookupEnv = original }()

	env := buildTransportEnv(map[string]string{"MY_SERVER_VAR": "configured"})

	if env["PATH"] != "/usr/bin" {
		t.Errorf("PATH not forwarded: %q", env["PATH"])
	}
	if env["HOME"] != "/home/tester" {
		t.Errorf("HOME not forwarded: %q", env["HOME"])
	}
	if env["MY_SERVER_VAR"] != "configured" {
		t.Errorf("configured variable not applied: %q", env["MY_SERVER_VAR"])
	}
	if _, leaked := env["AWS_SECRET_ACCESS_KEY"]; leaked {
		t.Error("a non-allowlisted host variable was forwarded to the child")
	}
}

func TestBuildTransportEnvSkipsMalformedVariableNames(t *testing.T) {
	// A name containing "=" or NUL would corrupt the environment block. Dropping it is
	// better than emitting something the child cannot parse.
	env := buildTransportEnv(map[string]string{
		"GOOD":         "value",
		"BAD=NAME":     "value",
		"NULL\x00NAME": "value",
		"":             "value",
	})

	if env["GOOD"] != "value" {
		t.Errorf("valid variable dropped: %q", env["GOOD"])
	}
	for _, bad := range []string{"BAD=NAME", "NULL\x00NAME", ""} {
		if _, ok := env[bad]; ok {
			t.Errorf("malformed variable name %q was accepted", bad)
		}
	}
}
