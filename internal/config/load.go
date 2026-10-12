package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Limits bound the resources a single configuration file may consume.
//
// These are defensive limits, not preferences. A config file is untrusted input: it may
// be enormous, may be a symlink to somewhere unexpected, or may not be readable at all.
// Every one of those cases has to fail in a bounded, explainable way.
type Limits struct {
	// MaxFileSize is the largest config file that will be read. Files above this are
	// reported rather than parsed, because reading an arbitrarily large file into
	// memory is a denial-of-service vector against the tool itself.
	MaxFileSize int64

	// MaxServers is the largest number of server entries accepted in one file.
	MaxServers int

	// MaxArgs and MaxEnv bound the size of a single server entry's arrays and objects.
	MaxArgs int
	MaxEnv  int

	// MaxHTTPRedirects bounds redirect following for Streamable HTTP endpoints.
	MaxHTTPRedirects int

	// MaxHTTPResponseBytes bounds how much of an HTTP response body is read.
	MaxHTTPResponseBytes int64
}

// DefaultLimits are the limits applied when a caller does not override them.
func DefaultLimits() Limits {
	return Limits{
		MaxFileSize:          4 << 20, // 4 MiB: far above any real MCP config
		MaxServers:           500,     //
		MaxArgs:              256,     //
		MaxEnv:               256,     //
		MaxHTTPRedirects:     3,       //
		MaxHTTPResponseBytes: 8 << 20, // 8 MiB
	}
}

// FileError describes why a configuration file could not be used.
//
// The Kind maps to a diagnostic code so that the caller does not have to string-match
// an error message to decide what to report.
type FileError struct {
	Kind string // one of the FileErrorKind constants

	// Path is the file the error concerns.
	Path string

	// Err is the underlying cause, wrapped for context.
	Err error

	// Size is the file's size when Kind is FileErrTooLarge.
	Size int64
}

// File error kinds, named to match the diagnostic codes they map to.
const (
	// FileErrNotFound means the file does not exist.
	FileErrNotFound = "not-found"

	// FileErrUnreadable means the file exists but could not be read.
	FileErrUnreadable = "unreadable"

	// FileErrTooLarge means the file exceeds MaxFileSize.
	FileErrTooLarge = "too-large"

	// FileErrInvalid means the file could not be parsed as JSON.
	FileErrInvalid = "invalid"

	// FileErrUnsupportedFormat means the file is a format MCP Doctor detects but does not
	// parse, such as YAML or TOML.
	FileErrUnsupportedFormat = "unsupported-format"
)

// Error implements error.
func (e *FileError) Error() string {
	switch e.Kind {
	case FileErrNotFound:
		return fmt.Sprintf("%s: no such file", e.Path)
	case FileErrTooLarge:
		return fmt.Sprintf("%s: file is too large to read safely", e.Path)
	default:
		return fmt.Sprintf("%s: %v", e.Path, e.Err)
	}
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *FileError) Unwrap() error { return e.Err }

// DiagnosticCode maps a file error kind to its stable diagnostic code.
//
// Mapping lives here rather than at the call site so that every path that reports a
// read failure produces the same code, which is what makes the code meaningful.
func (e *FileError) DiagnosticCode() string {
	switch e.Kind {
	case FileErrNotFound:
		return "MDC0001" // diagnose.CodeConfigNotFound
	case FileErrUnreadable:
		return "MDC0002" // diagnose.CodeConfigUnreadable
	case FileErrTooLarge:
		return "MDC0003" // diagnose.CodeConfigTooLarge
	case FileErrInvalid:
		return "MDC0004" // diagnose.CodeConfigNotJSON
	case FileErrUnsupportedFormat:
		return "MDC0011" // diagnose.CodeConfigUnsupportedFormat
	default:
		return "MDC0002"
	}
}

// FileResult is the outcome of reading one configuration file.
type FileResult struct {
	// Source describes where the file came from.
	Source Source

	// Node is the parsed document. It is nil when the file could not be read or parsed.
	Node *Node

	// Raw is the file bytes, retained so that validation can re-read positions and so
	// the report can quote the original text.
	Raw []byte

	// Err is the read or parse failure, if any. A non-nil Err does not mean the caller
	// should stop: `check` reports every file it can and summarises the rest.
	Err *FileError

	// Pos is where the parse failed, when Err.Kind is FileErrInvalid.
	Pos Position

	// SymlinkTarget is the resolved path when the config path is a symlink. Empty
	// otherwise.
	SymlinkTarget string
}

// Exists reports whether the file was present.
func (r FileResult) Exists() bool { return r.Err == nil || r.Err.Kind != FileErrNotFound }

// ReadFile reads and parses one configuration file under the given limits.
//
// The function is total: it returns a FileResult rather than a bare error so that a
// caller examining many configs can report on all of them instead of stopping at the
// first failure. Only genuinely unusable input, such as a path that is not a string,
// is handled by the caller.
//
// The file is opened read-only and never for writing: this tool never modifies user
// configuration, and the code reflects that.
func ReadFile(src Source, limits Limits) FileResult {
	res := FileResult{Source: src}

	if src.Format != FormatJSON {
		res.Err = &FileError{Kind: FileErrUnsupportedFormat, Path: src.Path, Err: fmt.Errorf("%s format is not supported in this release", src.Format)}
		return res
	}

	info, err := os.Stat(src.Path)
	if err != nil {
		res.Err = statError(src.Path, err)
		return res
	}
	if info.IsDir() {
		res.Err = &FileError{Kind: FileErrUnreadable, Path: src.Path, Err: errors.New("path is a directory, not a config file")}
		return res
	}
	if limits.MaxFileSize > 0 && info.Size() > limits.MaxFileSize {
		res.Err = &FileError{Kind: FileErrTooLarge, Path: src.Path, Size: info.Size(), Err: fmt.Errorf("exceeds the %d byte limit", limits.MaxFileSize)}
		return res
	}

	// Record symlink resolution so the report can tell the user which real file was read.
	// A symlinked config is legitimate, but in a shared directory it is also how a
	// config gets swapped underneath you, so it is worth surfacing.
	if link, lerr := os.Readlink(src.Path); lerr == nil {
		res.SymlinkTarget = resolveLink(src.Path, link)
	}

	f, err := os.Open(src.Path)
	if err != nil {
		res.Err = &FileError{Kind: FileErrUnreadable, Path: src.Path, Err: err}
		return res
	}
	defer f.Close()

	raw, err := readBounded(f, limits.MaxFileSize)
	if err != nil {
		res.Err = &FileError{Kind: FileErrUnreadable, Path: src.Path, Err: err}
		return res
	}
	res.Raw = raw

	node, perr := Parse(raw)
	if perr != nil {
		var pos Position
		var se *SyntaxError
		var pe *ParseError
		switch {
		case errors.As(perr, &se):
			pos = se.Pos
		case errors.As(perr, &pe):
			pos = pe.Pos
		}
		res.Err = &FileError{Kind: FileErrInvalid, Path: src.Path, Err: fmt.Errorf("%w at %s", perr, Location(src.Path, pos))}
		res.Pos = pos
		return res
	}

	res.Node = node
	return res
}

// Pos is the parse position recorded when Err is set, so a caller can render a precise
// location even though the error already carries one.
func (r FileResult) Position() Position {
	return r.Pos
}

// statError converts a stat failure into a classified FileError.
func statError(path string, err error) *FileError {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &FileError{Kind: FileErrNotFound, Path: path, Err: err}
	case errors.Is(err, fs.ErrPermission):
		return &FileError{Kind: FileErrUnreadable, Path: path, Err: fmt.Errorf("permission denied: %w", err)}
	default:
		return &FileError{Kind: FileErrUnreadable, Path: path, Err: err}
	}
}

// readBounded reads at most limit bytes, returning an error if more are available.
//
// Reading a known-size file with a limit of max+1 and then checking the length is what
// closes the gap between the stat size and the bytes actually read, which matters
// because the file can grow between the two.
func readBounded(f *os.File, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(f)
	}
	// Read one byte past the limit so an oversized file is detected rather than
	// silently truncated into invalid JSON.
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("file exceeds the %d byte limit", limit)
	}
	return raw, nil
}

// resolveLink resolves a symlink target to an absolute path.
func resolveLink(linkPath, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
}
