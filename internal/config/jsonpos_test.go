package config

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDecodesObjectsAndRecordsKeyPositions(t *testing.T) {
	src := `{
  "mcpServers": {
    "filesystem": {"command": "npx"}
  }
}`
	node, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	root, ok := node.Root.Obj()
	if !ok {
		t.Fatalf("root is %T, want map", node.Root.Value)
	}
	if _, present := root["mcpServers"]; !present {
		t.Errorf("decoded root missing mcpServers: %v", root)
	}

	// The whole point of this package: we must be able to say where a key was written.
	// Keys are recorded by JSON pointer, so that a duplicate is scoped to one object.
	pos, ok := node.KeyPositions["/mcpServers"]
	if !ok || len(pos) != 1 {
		t.Fatalf("KeyPositions[\"/mcpServers\"] = %v, want one entry", pos)
	}
	if pos[0].Line != 2 {
		t.Errorf("mcpServers recorded at line %d, want 2", pos[0].Line)
	}
	if pos[0].Col != 3 {
		t.Errorf("mcpServers recorded at col %d, want 3", pos[0].Col)
	}
}

func TestParseRecordsDuplicateKeysThatStandardParsersDrop(t *testing.T) {
	// encoding/json silently keeps the last duplicate, so this is the only place the
	// condition is observable. A merge accident is exactly what this catches.
	src := `{"command": "first", "command": "second"}`
	node, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	pos := node.KeyPositions["/command"]
	if len(pos) != 2 {
		t.Fatalf("KeyPositions[\"/command\"] has %d entries, want 2 (a duplicate)", len(pos))
	}
	if pos[0].Col == pos[1].Col {
		t.Errorf("duplicate positions not distinct: %v", pos)
	}
}

func TestParseDoesNotReportSameKeyNameInDifferentObjectsAsDuplicate(t *testing.T) {
	// Every MCP config has "command" in several servers. Those are not duplicates, and
	// a flat key-to-positions map would report every one of them, which is the bug this
	// test guards against.
	src := `{"mcpServers":{
		"a": {"command": "npx"},
		"b": {"command": "uvx"},
		"c": {"command": "node"}
	}}`
	node, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	for pointer, positions := range node.KeyPositions {
		if len(positions) > 1 {
			t.Errorf("pointer %q recorded %d positions, want 1 (same key name in different objects is not a duplicate)", pointer, len(positions))
		}
	}

	for _, want := range []string{"/mcpServers/a/command", "/mcpServers/b/command", "/mcpServers/c/command"} {
		if _, ok := node.KeyPositions[want]; !ok {
			t.Errorf("expected pointer %q to be recorded; got keys %v", want, keysOf(node))
		}
	}
}

func TestParseEscapesPointerSegmentsContainingSlashes(t *testing.T) {
	// A server named "a/b" must not produce the same pointer as a nested "a" -> "b".
	src := `{"mcpServers":{"a/b":{"command":"x"},"a":{"b":{"command":"y"}}}}`
	node, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if _, ok := node.KeyPositions["/mcpServers/a~1b/command"]; !ok {
		t.Errorf("slash in key not escaped per RFC 6901; keys: %v", keysOf(node))
	}
	if _, ok := node.KeyPositions["/mcpServers/a/b/command"]; !ok {
		t.Errorf("nested pointer missing; keys: %v", keysOf(node))
	}
}

// keysOf returns a node's recorded pointers, for failure messages.
func keysOf(n *Node) []string {
	out := make([]string, 0, len(n.KeyPositions))
	for k := range n.KeyPositions {
		out = append(out, k)
	}
	return out
}

func TestParseReportsLineAndColumnForMalformedJSON(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"missing comma", `{"a": 1 "b": 2}`},
		{"unterminated object", `{"a": 1`},
		{"unterminated string", `{"a": "unterminated`},
		{"unterminated array", `[1, 2`},
		{"bare token", `{"a": }`},
		{"trailing comma", `{"a": 1,}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatal("Parse succeeded on malformed input, want an error")
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("error is %T, want *SyntaxError", err)
			}
			if se.Pos.Line < 1 || se.Pos.Col < 1 {
				t.Errorf("position = %+v, want a real 1-based line and column", se.Pos)
			}
			if se.Msg == "" {
				t.Error("error message is empty")
			}
		})
	}
}

func TestParsePointsAtTheOffendingLineNotJustTheFile(t *testing.T) {
	src := `{
  "mcpServers": {
    "good": {"command": "npx"},
    "bad": {"command": }
  }
}`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	var se *SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T, want *SyntaxError", err)
	}
	if se.Pos.Line != 4 {
		t.Errorf("error line = %d, want 4 (the line with the missing value)", se.Pos.Line)
	}
}

func TestParseRejectsTrailingContent(t *testing.T) {
	// `{"a":1} garbage` is not a valid document. Ignoring the tail would hide real
	// corruption, such as two files concatenated.
	if _, err := Parse([]byte(`{"a":1} trailing`)); err == nil {
		t.Fatal("Parse accepted trailing content, want an error")
	}
}

func TestParseRejectsEmptyInput(t *testing.T) {
	if _, err := Parse([]byte("")); err == nil {
		t.Fatal("Parse accepted empty input, want an error")
	}
}

func TestParseNormalizesNumbersToFloats(t *testing.T) {
	// Using json.Number internally would force every validation rule to handle two
	// number types. Normalising once keeps the type switches total.
	node, err := Parse([]byte(`{"timeout": 30}`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	obj, _ := node.Root.Obj()
	if _, ok := obj["timeout"].(float64); !ok {
		t.Errorf("timeout is %T, want float64", obj["timeout"])
	}
}

func TestLocatedAccessorsAreTypeSafe(t *testing.T) {
	loc := Located{Value: map[string]any{
		"s":   "text",
		"b":   true,
		"n":   float64(5),
		"arr": []any{1.0, 2.0},
	}}

	if s, ok := loc.lookupForTest("s"); !ok || s != "text" {
		t.Errorf("Str = (%q, %v), want (\"text\", true)", s, ok)
	}
	if b, ok := Lookup(loc, "b"); !ok {
		t.Errorf("Bool lookup failed")
	} else if v, ok := b.Bool(); !ok || !v {
		t.Errorf("Bool = (%v, %v), want (true, true)", v, ok)
	}
	if n, ok := Lookup(loc, "n"); !ok {
		t.Errorf("Number lookup failed")
	} else if v, ok := n.Number(); !ok || v != 5 {
		t.Errorf("Number = (%v, %v), want (5, true)", v, ok)
	}
	if a, ok := Lookup(loc, "arr"); !ok {
		t.Errorf("Arr lookup failed")
	} else if v, ok := a.Arr(); !ok || len(v) != 2 {
		t.Errorf("Arr = (%v, %v), want 2 elements", v, ok)
	}

	// A wrong-type access must report false, never panic or coerce.
	if _, ok := Lookup(loc, "missing"); ok {
		t.Error("Lookup of a missing key reported present")
	}
	if _, ok := Lookup(Located{Value: "not an object"}, "s"); ok {
		t.Error("Lookup on a non-object reported present")
	}
}

// lookupForTest is a small helper that unwraps a string field for assertions.
func (l Located) lookupForTest(key string) (string, bool) {
	c, ok := Lookup(l, key)
	if !ok {
		return "", false
	}
	return c.Str()
}

func TestPositionStringAndLocationFormat(t *testing.T) {
	p := Position{Line: 12, Col: 5}
	if got := p.String(); got != "12:5" {
		t.Errorf("Position.String() = %q, want \"12:5\"", got)
	}
	if got := Location("/tmp/config.json", p); got != "/tmp/config.json:12:5" {
		t.Errorf("Location = %q, want \"/tmp/config.json:12:5\"", got)
	}
	// An unknown position must not render as ":0:0", which would be actively
	// misleading in a diagnostic.
	if got := Location("/tmp/config.json", Position{}); got != "/tmp/config.json" {
		t.Errorf("Location with zero position = %q, want the bare path", got)
	}
}

func TestOffsetToPositionClampsOutOfRangeOffsets(t *testing.T) {
	src := []byte("{\n  \"a\": 1\n}")
	if got := offsetToPosition(src, -5); got != (Position{Line: 1, Col: 1}) {
		t.Errorf("negative offset = %+v, want 1:1", got)
	}
	if got := offsetToPosition(src, 9999); got.Line != 3 {
		t.Errorf("oversized offset line = %d, want 3", got.Line)
	}
}

func TestParseHandlesNestedStructuresAndEscapes(t *testing.T) {
	src := `{"a": {"b": [1, {"c": "has \"escapes\" and , commas"}]}}`
	node, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error on nested escaped JSON: %v", err)
	}
	root, _ := node.Root.Obj()
	a, _ := Lookup(node.Root, "a")
	b, _ := Lookup(a, "b")
	arr, ok := b.Arr()
	if !ok || len(arr) != 2 {
		t.Fatalf("nested array not decoded: %v", arr)
	}
	if _, present := root["a"]; !present {
		t.Error("top-level key missing after nested parse")
	}
}

func TestParseHandlesUnicodeContent(t *testing.T) {
	// Byte offsets and rune positions diverge for multibyte text; the scanner must not
	// panic or lose track of lines.
	src := "{\n  \"name\": \"服务器 🚀\"\n}"
	node, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse error on unicode: %v", err)
	}
	pos := node.KeyPositions["/name"]
	if len(pos) != 1 || pos[0].Line != 2 {
		t.Errorf("unicode key position = %v, want line 2", pos)
	}
}

func TestParseErrorMessageDoesNotLeakRawOffsets(t *testing.T) {
	// The rendered message should read naturally after the position we print in front of
	// it, so encoding/json's "(offset N)" tail is stripped.
	_, err := Parse([]byte(`{"a" 1}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "offset ") {
		t.Errorf("error message leaks a raw byte offset: %q", msg)
	}
	if strings.Contains(msg, "json: ") {
		t.Errorf("error message retains encoding/json prefix: %q", msg)
	}
}

func TestParseSkipsWhitespaceIncludingBeforeColon(t *testing.T) {
	if _, err := Parse([]byte("{  \"a\"  :  1  }")); err != nil {
		t.Errorf("Parse failed on legal whitespace: %v", err)
	}
}
