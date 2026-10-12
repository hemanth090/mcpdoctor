package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFile creates a file with the given contents inside dir.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// jsonSource builds a Source for a path with the JSON format.
func jsonSource(path string) Source {
	return Source{Client: ClientClaudeDesktop, DisplayName: "Claude Desktop", Path: path, Scope: ScopeUser, Format: FormatJSON}
}

func TestReadFileParsesValidConfig(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.json", `{"mcpServers":{"a":{"command":"npx"}}}`)

	res := ReadFile(jsonSource(p), DefaultLimits())
	if res.Err != nil {
		t.Fatalf("ReadFile error: %v", res.Err)
	}
	if res.Node == nil {
		t.Fatal("Node is nil for a valid config")
	}
	if len(res.Raw) == 0 {
		t.Error("Raw bytes were not retained")
	}
}

func TestReadFileMissingFileIsNotFound(t *testing.T) {
	res := ReadFile(jsonSource(filepath.Join(t.TempDir(), "absent.json")), DefaultLimits())
	if res.Err == nil {
		t.Fatal("ReadFile on a missing file returned no error")
	}
	if res.Err.Kind != FileErrNotFound {
		t.Errorf("Kind = %q, want %q", res.Err.Kind, FileErrNotFound)
	}
	if res.Err.DiagnosticCode() != "MDC0001" {
		t.Errorf("DiagnosticCode = %q, want MDC0001", res.Err.DiagnosticCode())
	}
}

func TestReadFileDirectoryIsUnreadable(t *testing.T) {
	// A user who points --config at a directory should get a clear message, not a
	// confusing parse error.
	dir := t.TempDir()
	res := ReadFile(jsonSource(dir), DefaultLimits())
	if res.Err == nil {
		t.Fatal("ReadFile on a directory returned no error")
	}
	if res.Err.Kind != FileErrUnreadable {
		t.Errorf("Kind = %q, want %q", res.Err.Kind, FileErrUnreadable)
	}
	if !strings.Contains(res.Err.Error(), "directory") {
		t.Errorf("error should mention it is a directory, got %q", res.Err.Error())
	}
}

func TestReadFileMalformedJSONCarriesPosition(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.json", "{\n  \"mcpServers\": {\n    \"a\": {bad}\n  }\n}")

	res := ReadFile(jsonSource(p), DefaultLimits())
	if res.Err == nil {
		t.Fatal("ReadFile on malformed JSON returned no error")
	}
	if res.Err.Kind != FileErrInvalid {
		t.Errorf("Kind = %q, want %q", res.Err.Kind, FileErrInvalid)
	}
	if res.Pos.Line < 1 {
		t.Errorf("parse position = %+v, want a real line", res.Pos)
	}
	// The error message must include the file and line so the user can jump to it.
	if !strings.Contains(res.Err.Error(), p) {
		t.Errorf("error does not name the file: %q", res.Err.Error())
	}
}

func TestReadFileEnforcesSizeLimit(t *testing.T) {
	dir := t.TempDir()
	// A file larger than the limit must be refused, not read into memory.
	big := `{"padding":"` + strings.Repeat("A", 4096) + `"}`
	p := writeFile(t, dir, "big.json", big)

	limits := DefaultLimits()
	limits.MaxFileSize = 100

	res := ReadFile(jsonSource(p), limits)
	if res.Err == nil {
		t.Fatal("oversized file was read without an error")
	}
	if res.Err.Kind != FileErrTooLarge {
		t.Errorf("Kind = %q, want %q", res.Err.Kind, FileErrTooLarge)
	}
	if res.Err.DiagnosticCode() != "MDC0003" {
		t.Errorf("DiagnosticCode = %q, want MDC0003", res.Err.DiagnosticCode())
	}
}

func TestReadFileSizeLimitUsesStatBeforeReading(t *testing.T) {
	// The limit must be enforced from the file's size, not by discovering the overflow
	// after allocating. A file that grows between stat and read is also caught by the
	// bounded reader, but the cheap path is the common one.
	dir := t.TempDir()
	p := writeFile(t, dir, "big.json", strings.Repeat("A", 2048))

	limits := DefaultLimits()
	limits.MaxFileSize = 512

	res := ReadFile(jsonSource(p), limits)
	if res.Err == nil || res.Err.Kind != FileErrTooLarge {
		t.Fatalf("expected too-large, got %+v", res.Err)
	}
	if len(res.Raw) != 0 {
		t.Errorf("Raw should be empty for a refused file, got %d bytes", len(res.Raw))
	}
}

func TestReadFileUnreadablePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	dir := t.TempDir()
	p := writeFile(t, dir, "secret.json", `{"mcpServers":{}}`)
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	res := ReadFile(jsonSource(p), DefaultLimits())
	if res.Err == nil {
		t.Fatal("unreadable file returned no error")
	}
	if res.Err.Kind != FileErrUnreadable {
		t.Errorf("Kind = %q, want %q", res.Err.Kind, FileErrUnreadable)
	}
	if res.Err.DiagnosticCode() != "MDC0002" {
		t.Errorf("DiagnosticCode = %q, want MDC0002", res.Err.DiagnosticCode())
	}
}

func TestReadFileReportsSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	dir := t.TempDir()
	real := writeFile(t, dir, "real.json", `{"mcpServers":{}}`)
	link := filepath.Join(dir, "link.json")

	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	res := ReadFile(jsonSource(link), DefaultLimits())
	if res.Err != nil {
		t.Fatalf("reading a symlink failed: %v", res.Err)
	}
	if res.SymlinkTarget != real {
		t.Errorf("SymlinkTarget = %q, want %q", res.SymlinkTarget, real)
	}
}

func TestReadFileRejectsUnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "mcpServers:\n  a:\n    command: npx\n")

	src := Source{Path: p, Format: FormatYAML}
	res := ReadFile(src, DefaultLimits())
	if res.Err == nil {
		t.Fatal("YAML config was not rejected")
	}
	if res.Err.Kind != FileErrUnsupportedFormat {
		t.Errorf("Kind = %q, want %q", res.Err.Kind, FileErrUnsupportedFormat)
	}
	if res.Err.DiagnosticCode() != "MDC0011" {
		t.Errorf("DiagnosticCode = %q, want MDC0011", res.Err.DiagnosticCode())
	}
}

func TestReadBoundedRefusesExactlyLimitPlusOne(t *testing.T) {
	// The bounded reader must detect a file that is one byte over the limit, which is
	// the boundary a naive implementation gets wrong.
	dir := t.TempDir()
	p := writeFile(t, dir, "exact.json", strings.Repeat("x", 101))

	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, err := readBounded(f, 100); err == nil {
		t.Error("readBounded accepted a file one byte over the limit")
	}
}

func TestReadBoundedAcceptsExactlyTheLimit(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "exact.json", strings.Repeat("x", 100))

	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	data, err := readBounded(f, 100)
	if err != nil {
		t.Errorf("readBounded rejected a file exactly at the limit: %v", err)
	}
	if len(data) != 100 {
		t.Errorf("read %d bytes, want 100", len(data))
	}
}

func TestReadBoundedWithNonPositiveLimitReadsEverything(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "f.json", strings.Repeat("x", 500))

	f, _ := os.Open(p)
	defer f.Close()

	data, err := readBounded(f, 0)
	if err != nil {
		t.Fatalf("readBounded errored: %v", err)
	}
	if len(data) != 500 {
		t.Errorf("read %d bytes, want 500", len(data))
	}
}

func TestFileErrorCodesAreStableAndDistinct(t *testing.T) {
	// These map into the documented diagnostic code set, so a change here would silently
	// break anyone's automation that matches on codes.
	cases := map[string]string{
		FileErrNotFound:          "MDC0001",
		FileErrUnreadable:        "MDC0002",
		FileErrTooLarge:          "MDC0003",
		FileErrInvalid:           "MDC0004",
		FileErrUnsupportedFormat: "MDC0011",
	}
	for kind, want := range cases {
		e := &FileError{Kind: kind, Path: "x"}
		if got := e.DiagnosticCode(); got != want {
			t.Errorf("kind %q code = %q, want %q", kind, got, want)
		}
	}
}

func TestDescribePathsIsDeterministicAndDeduplicated(t *testing.T) {
	first := DescribePaths("/some/project")
	for i := 0; i < 10; i++ {
		next := DescribePaths("/some/project")
		if len(next) != len(first) {
			t.Fatalf("run %d returned %d paths, want %d", i, len(next), len(first))
		}
		for j := range first {
			if next[j].Path != first[j].Path {
				t.Fatalf("run %d path %d = %q, want %q", i, j, next[j].Path, first[j].Path)
			}
		}
	}

	seen := make(map[string]bool)
	for _, s := range first {
		if seen[s.Path] {
			t.Errorf("path %q appears more than once", s.Path)
		}
		seen[s.Path] = true
	}
}

func TestDescribePathsIncludesProjectConfigsOnlyWhenRootGiven(t *testing.T) {
	withRoot := DescribePaths("/project")
	hasProject := false
	for _, s := range withRoot {
		if s.Scope == ScopeProject {
			hasProject = true
			break
		}
	}
	if !hasProject {
		t.Error("project-scoped configs missing when a project root was given")
	}

	withoutRoot := DescribePaths("")
	for _, s := range withoutRoot {
		if s.Scope == ScopeProject {
			t.Errorf("project-scoped config %q returned without a project root", s.Path)
		}
	}
}

func TestAdaptersReturnsACopySoCallersCannotMutateTheRegistry(t *testing.T) {
	a := Adapters()
	if len(a) == 0 {
		t.Fatal("no adapters registered")
	}
	original := a[0].Client
	a[0].Client = "tampered"

	if Adapters()[0].Client != original {
		t.Error("mutating the returned slice changed the registry")
	}
}

func TestAdapterForReturnsFalseForUnknownClient(t *testing.T) {
	if _, ok := AdapterFor("not-a-real-client"); ok {
		t.Error("AdapterFor returned true for an unknown client")
	}
}

func TestUserPathsNeverReturnEmptyStrings(t *testing.T) {
	// An empty path would probe the current working directory, which is a bug rather
	// than a miss.
	for _, a := range Adapters() {
		for _, p := range a.UserPaths() {
			if strings.TrimSpace(p) == "" {
				t.Errorf("client %s returned an empty user path", a.Client)
			}
		}
	}
}

func TestUserPathsAreDeduplicated(t *testing.T) {
	// Several clients resolve to the same file on some platforms; the user should be
	// told about it once.
	for _, a := range Adapters() {
		seen := make(map[string]bool)
		for _, p := range a.UserPaths() {
			if seen[p] {
				t.Errorf("client %s returned duplicate path %q", a.Client, p)
			}
			seen[p] = true
		}
	}
}

func TestIsProjectPathRecognizesUnambiguousProjectLayouts(t *testing.T) {
	// Only unambiguous markers are asserted. The project config for Cursor lives at
	// <repo>/.cursor/mcp.json and the global one at ~/.cursor/mcp.json; given only a
	// path with no project root, the two cannot be told apart, so the function declines
	// to guess rather than guessing wrong.
	project := []string{
		"/repo/.mcp.json",
		"/repo/.vscode/mcp.json",
		"/repo/.zed/settings.json",
		"/repo/.roo/mcp.json",
		"/repo/.amazonq/mcp.json",
		"/repo/.gemini/settings.json",
	}
	for _, p := range project {
		if !IsProjectPath(p) {
			t.Errorf("IsProjectPath(%q) = false, want true", p)
		}
	}

	user := []string{
		"/home/alice/Library/Application Support/Claude/claude_desktop_config.json",
		"/home/alice/.config/zed/settings.json",
		"/home/alice/.gemini/settings.json.notes",
	}
	for _, p := range user {
		if IsProjectPath(p) {
			t.Errorf("IsProjectPath(%q) = true, want false", p)
		}
	}
}

func TestIsProjectPathDeclinesAmbiguousCases(t *testing.T) {
	// Documented behaviour: a path that could be either, the function returns false
	// rather than guessing. Guessing "project" for a home directory would wrongly warn
	// a user about their own config.
	for _, p := range []string{"/home/alice/.cursor/mcp.json", "/repo/.cursor/mcp.json"} {
		if IsProjectPath(p) {
			t.Errorf("IsProjectPath(%q) = true; ambiguous cases must not be classified", p)
		}
	}
}

func TestIsAbsolutePathHandlesPlatformForms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX absolute paths are not absolute on Windows")
	}
	if !isAbsolutePath("/usr/local/bin/server") {
		t.Error("POSIX absolute path not recognised")
	}
	if isAbsolutePath("npx") {
		t.Error("bare name treated as absolute")
	}
	if isAbsolutePath("") {
		t.Error("empty string treated as absolute")
	}
}
