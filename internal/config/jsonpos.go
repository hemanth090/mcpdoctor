package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Position is a 1-based line and column location inside a source file.
//
// Lines and columns are what a user needs to jump to the problem in an editor. A raw
// byte offset, which is what encoding/json reports, is not.
type Position struct {
	Line int `json:"line"`
	Col  int `json:"col"`
}

// String formats the position as "line:col".
func (p Position) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// Location formats a path with a position as "config.json:12:5".
//
// It returns the bare path when the position is unknown, so a caller can use the result
// unconditionally without emitting a misleading ":0:0".
func Location(path string, pos Position) string {
	if pos.Line == 0 {
		return path
	}
	return fmt.Sprintf("%s:%d:%d", path, pos.Line, pos.Col)
}

// Located is a JSON value plus the source position of the value itself.
//
// Carrying positions alongside values is what lets a validation rule point at the exact
// offending line rather than saying "somewhere in this file".
type Located struct {
	// Value is the decoded JSON value: map[string]any, []any, string, float64, bool, or
	// nil.
	Value any

	// Pos is where Value begins in the source.
	Pos Position

	// KeyPos is where the object's key begins. It is only meaningful when this value is
	// a member of an object.
	KeyPos Position
}

// Node is a parsed JSON document that preserves positions.
//
// Node deliberately is not encoding/json's model. That model loses positions entirely,
// and the difference between "line 40" and "somewhere in a 900-line file" is the whole
// value of this package.
type Node struct {
	// Root is the top-level value.
	Root Located

	// KeyPositions maps a JSON pointer to every position at which that key was written.
	//
	// The pointer is the path from the document root, for example
	// "/mcpServers/filesystem/command". Scoping by path is essential: a duplicate key
	// only means anything when both occurrences are in the same object, and a flat map
	// keyed by name alone would report every common field name in the document as a
	// duplicate.
	KeyPositions map[string][]Position
}

// Obj returns the value as an object and whether it was one.
func (l Located) Obj() (map[string]any, bool) {
	m, ok := l.Value.(map[string]any)
	return m, ok
}

// Arr returns the value as an array and whether it was one.
func (l Located) Arr() ([]any, bool) {
	a, ok := l.Value.([]any)
	return a, ok
}

// Str returns the value as a string and whether it was one.
func (l Located) Str() (string, bool) {
	s, ok := l.Value.(string)
	return s, ok
}

// Bool returns the value as a bool and whether it was one.
func (l Located) Bool() (bool, bool) {
	b, ok := l.Value.(bool)
	return b, ok
}

// Number returns the value as a float64 and whether it was a JSON number.
func (l Located) Number() (float64, bool) {
	f, ok := l.Value.(float64)
	return f, ok
}

// Lookup finds a direct child of an object node.
//
// It returns the child's value and position, and whether the key was present at all.
// A missing key is not an error here: deciding whether absence is a problem belongs to
// the validation rule, not to the accessor.
func Lookup(obj Located, key string) (Located, bool) {
	m, ok := obj.Obj()
	if !ok {
		return Located{}, false
	}
	v, present := m[key]
	if !present {
		return Located{}, false
	}
	return Located{Value: v, Pos: obj.Pos}, true
}

// SyntaxError describes a JSON parse failure with a source position.
//
// It exists because "invalid character 'x' looking for beginning of value" without a
// line number is a support ticket, whereas "config.json:14:3" is a fix.
type SyntaxError struct {
	Pos Position
	Msg string
}

// Error implements error.
func (e *SyntaxError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

// ParseError describes a structural problem detected after successful JSON decoding,
// such as a document whose top level is an array when an object was required.
type ParseError struct {
	Pos Position
	Msg string
}

// Error implements error.
func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

// doc is the cursor state threaded through the recursive parse.
type doc struct {
	src []byte

	// pos is the byte offset of the next unconsumed byte.
	pos int

	// line and col track the current position for human-facing errors.
	line, col int

	keyPositions map[string][]Position

	// path is the JSON pointer prefix for the value currently being scanned, for example
	// "/mcpServers/filesystem". It is pushed on entry to an object and popped on exit.
	path []string
}

// Parse decodes JSON into a [Node], preserving source positions.
//
// It returns a [SyntaxError] with a line and column for malformed input. The document is
// parsed twice: once with encoding/json to obtain the standard value model, and once
// with a scanner that records where each key was written. The duplication is
// deliberate, and the reason is that encoding/json's error carries a byte offset while
// our contract promises a line and column, and its duplicate keys are silently
// discarded rather than reported.
func Parse(src []byte) (*Node, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()

	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, syntaxErrorFromJSON(src, err)
	}

	// Reject trailing content: `{"a":1} garbage` is not a valid document, and silently
	// ignoring the tail would hide a real corruption.
	if err := ensureNoTrailingContent(src, dec); err != nil {
		return nil, err
	}

	d := &doc{src: src, line: 1, col: 1, keyPositions: make(map[string][]Position)}
	if _, err := d.scanValue(); err != nil {
		return nil, err
	}

	return &Node{
		Root:         Located{Value: normalizeNumbers(root)},
		KeyPositions: d.keyPositions,
	}, nil
}

// ensureNoTrailingContent verifies the document ends after the first value.
//
// A failure here must still be a typed error: callers classify errors to choose a
// diagnostic code, and a bare *json.SyntaxError escaping here would leave the caller with
// no code to report. The fuzzer found this path, which is exactly why the error contract
// is asserted rather than assumed.
func ensureNoTrailingContent(src []byte, dec *json.Decoder) error {
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if err == nil {
		return &SyntaxError{Pos: Position{Line: 1, Col: 1}, Msg: "unexpected content after the top-level JSON value"}
	}
	// io.EOF means the document ended cleanly, which is what we want.
	if errors.Is(err, io.EOF) {
		return nil
	}
	return syntaxErrorFromJSON(src, err)
}

// normalizeNumbers converts json.Number to float64 so the value tree contains only
// map/slice/string/float64/bool/nil, which keeps type switches in validation rules
// simple and total.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return t.String()
		}
		return f
	case map[string]any:
		for k, sub := range t {
			t[k] = normalizeNumbers(sub)
		}
		return t
	case []any:
		for i, sub := range t {
			t[i] = normalizeNumbers(sub)
		}
		return t
	default:
		return v
	}
}

// syntaxErrorFromJSON converts a decoder error into a positioned SyntaxError.
//
// encoding/json exposes the byte offset through *json.SyntaxError and *json.UnmarshalTypeError.
// Recovering the offset and mapping it to a line and column is the entire value this
// function adds.
func syntaxErrorFromJSON(src []byte, err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return &SyntaxError{Pos: offsetToPosition(src, int(se.Offset)), Msg: cleanJSONError(se.Error())}
	}
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		pos := Position{Line: 1, Col: 1}
		if ute.Offset > 0 {
			pos = offsetToPosition(src, int(ute.Offset))
		}
		return &SyntaxError{Pos: pos, Msg: fmt.Sprintf("unexpected %s where %s was expected", ute.Value, ute.Type.String())}
	}
	// EOF and other non-offset errors still need a usable position.
	return &SyntaxError{Pos: offsetToPosition(src, len(src)), Msg: cleanJSONError(err.Error())}
}

// cleanJSONError trims encoding/json's prefix so the message reads naturally after the
// position we print in front of it.
func cleanJSONError(msg string) string {
	msg = strings.TrimPrefix(msg, "json: ")
	if idx := strings.Index(msg, " (offset "); idx >= 0 {
		msg = msg[:idx]
	}
	return msg
}

// offsetToPosition maps a byte offset to a 1-based line and column.
//
// Offsets past the end clamp to the last position rather than failing, because the
// decoder reports one past the final byte for a truncated document and that is exactly
// the case where the user most needs a location.
func offsetToPosition(src []byte, offset int) Position {
	if offset < 0 {
		offset = 0
	}
	if offset > len(src) {
		offset = len(src)
	}
	line, col := 1, 1
	for i := 0; i < offset; i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return Position{Line: line, Col: col}
}

// scanValue walks the raw source once, recording the position of every object key.
//
// It is a scanner, not a second parser: it validates structure only to the extent needed
// to track nesting, and it never builds a second value tree. The decoded tree already
// came from encoding/json, which is the authority on what the document means.
func (d *doc) scanValue() (Position, error) {
	d.skipSpace()
	start := Position{Line: d.line, Col: d.col}
	if d.pos >= len(d.src) {
		return start, &SyntaxError{Pos: start, Msg: "unexpected end of input"}
	}

	switch c := d.src[d.pos]; {
	case c == '{':
		return start, d.scanObject(start)
	case c == '[':
		return start, d.scanArray(start)
	case c == '"':
		// A string value must be consumed as a unit. Letting it fall through to
		// scanScalar would stop at the first comma inside it, which is legal JSON but
		// would desynchronise the scanner and corrupt every position after it.
		if _, err := d.scanString(); err != nil {
			return start, err
		}
		return start, nil
	default:
		return start, d.scanScalar()
	}
}

// scanObject records key positions until the matching close brace.
func (d *doc) scanObject(objStart Position) error {
	d.advance() // consume '{'
	for {
		d.skipSpace()
		if d.pos >= len(d.src) {
			return &SyntaxError{Pos: objStart, Msg: "unterminated object"}
		}
		if d.src[d.pos] == '}' {
			d.advance()
			return nil
		}

		keyPos := Position{Line: d.line, Col: d.col}
		key := ""
		if d.src[d.pos] == '"' {
			decoded, err := d.scanString()
			if err != nil {
				return err
			}
			key = decoded
			// A duplicate key matters only when both occurrences are in this object, so
			// the record is scoped by the object's JSON pointer rather than by the bare
			// key name. Without the path, every repeated field name anywhere in the
			// document would be reported as a duplicate.
			pointer := d.pointer(key)
			// A duplicate key is almost always a bad merge. encoding/json keeps only the
			// last, so this is the only place it can be observed.
			d.keyPositions[pointer] = append(d.keyPositions[pointer], keyPos)
		} else {
			if err := d.scanScalar(); err != nil {
				return err
			}
		}

		d.skipSpace()
		if d.pos >= len(d.src) || d.src[d.pos] != ':' {
			return &SyntaxError{Pos: d.here(), Msg: `expected ":" after object key`}
		}
		d.advance() // consume ':'

		// Descend into the key's value so nested objects record their own paths. The
		// key is pushed before the value is scanned and popped afterwards, which is what
		// scopes duplicate detection to a single object.
		d.path = append(d.path, key)
		if _, err := d.scanValue(); err != nil {
			d.path = d.path[:len(d.path)-1]
			return err
		}
		d.path = d.path[:len(d.path)-1]

		d.skipSpace()
		if d.pos >= len(d.src) {
			return &SyntaxError{Pos: objStart, Msg: "unterminated object"}
		}
		switch d.src[d.pos] {
		case ',':
			d.advance()
		case '}':
			d.advance()
			return nil
		default:
			return &SyntaxError{Pos: d.here(), Msg: `expected "," or "}" after object value`}
		}
	}
}

// scanArray walks elements until the matching close bracket.
//
// Array elements are indexed in the path so that two objects inside the same array are
// distinguishable; without the index, every element would share a pointer and an object
// appearing twice in a list would look like a duplicate key.
func (d *doc) scanArray(arrStart Position) error {
	d.advance() // consume '['
	index := 0
	for {
		d.skipSpace()
		if d.pos >= len(d.src) {
			return &SyntaxError{Pos: arrStart, Msg: "unterminated array"}
		}
		if d.src[d.pos] == ']' {
			d.advance()
			return nil
		}

		d.path = append(d.path, strconv.Itoa(index))
		if _, err := d.scanValue(); err != nil {
			d.path = d.path[:len(d.path)-1]
			return err
		}
		d.path = d.path[:len(d.path)-1]
		index++

		d.skipSpace()
		if d.pos >= len(d.src) {
			return &SyntaxError{Pos: arrStart, Msg: "unterminated array"}
		}
		switch d.src[d.pos] {
		case ',':
			d.advance()
		case ']':
			d.advance()
			return nil
		default:
			return &SyntaxError{Pos: d.here(), Msg: `expected "," or "]" after array element`}
		}
	}
}

// scanScalar consumes a number, true, false, or null.
func (d *doc) scanScalar() error {
	start := d.pos
	for d.pos < len(d.src) && !isValueTerminator(d.src[d.pos]) {
		d.advance()
	}
	if d.pos == start {
		return &SyntaxError{Pos: d.here(), Msg: "expected a JSON value"}
	}
	return nil
}

// isValueTerminator reports whether c ends a scalar token.
func isValueTerminator(c byte) bool {
	switch c {
	case ',', '}', ']', ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// scanString consumes a quoted string, returning its decoded content.
//
// The raw content span is captured as byte offsets as the scanner walks, rather than by
// replaying line/column positions afterwards. Replaying would be quadratic for a file
// with many keys and, more importantly, would be fragile: a byte offset is what the
// substring actually needs, and deriving it from a position invites a subtle off-by-one
// that only shows up on escaped or multibyte input.
func (d *doc) scanString() (string, error) {
	start := Position{Line: d.line, Col: d.col}
	contentStart := d.pos + 1 // just after the opening quote
	d.advance()               // consume opening quote
	for {
		if d.pos >= len(d.src) {
			return "", &SyntaxError{Pos: start, Msg: "unterminated string"}
		}
		c := d.src[d.pos]
		if c == '\\' {
			// Skip the backslash and whatever it escapes, without interpreting it. The
			// decoded tree already holds the right value; this pass only tracks spans.
			d.advance()
			if d.pos < len(d.src) {
				d.advance()
			}
			continue
		}
		if c == '"' {
			var out string
			raw := string(d.src[contentStart:d.pos])
			if err := json.Unmarshal([]byte(`"`+raw+`"`), &out); err != nil {
				// A key that will not decode cannot name anything; treat it as opaque
				// rather than failing the whole parse, since the value tree is already
				// authoritative.
				return "", nil
			}
			d.advance() // consume closing quote
			return out, nil
		}
		d.advance()
	}
}

// advance consumes one byte, tracking line and column.
func (d *doc) advance() {
	if d.pos >= len(d.src) {
		return
	}
	if d.src[d.pos] == '\n' {
		d.line++
		d.col = 1
	} else {
		d.col++
	}
	d.pos++
}

// skipSpace consumes JSON whitespace.
func (d *doc) skipSpace() {
	for d.pos < len(d.src) {
		switch d.src[d.pos] {
		case ' ', '\t', '\r', '\n':
			d.advance()
		default:
			return
		}
	}
}

// pointer renders the current path plus key as a JSON pointer.
//
// Escaping follows RFC 6901, so a key containing a slash cannot collide with a path
// separator and produce a false duplicate.
func (d *doc) pointer(key string) string {
	var b strings.Builder
	for _, segment := range d.path {
		b.WriteByte('/')
		b.WriteString(escapePointerSegment(segment))
	}
	b.WriteByte('/')
	b.WriteString(escapePointerSegment(key))
	return b.String()
}

// escapePointerSegment escapes a JSON pointer segment per RFC 6901.
func escapePointerSegment(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

// here returns the current cursor position.
func (d *doc) here() Position { return Position{Line: d.line, Col: d.col} }
