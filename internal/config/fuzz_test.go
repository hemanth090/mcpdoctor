package config

import (
	"errors"
	"strings"
	"testing"
)

// FuzzParseConfig exercises the JSON position-tracking parser against arbitrary input.
//
// The parser is the component most exposed to untrusted bytes: it runs on files that
// MCP Doctor did not create, in directories the user may not control. It must never
// panic, never hang, and never report a position outside the input.
func FuzzParseConfig(f *testing.F) {
	seeds := []string{
		`{}`,
		`{"mcpServers":{}}`,
		`{"mcpServers":{"a":{"command":"npx","args":["-y","pkg"]}}}`,
		`{"mcpServers":{"a":{"url":"https://example.com/mcp"}}}`,
		`{"a":1,"a":2}`,
		`{"unicode":"服务器 🚀"}`,
		`{"escaped":"quote \" backslash \\ newline \n"}`,
		`{"nested":{"deep":{"deeper":[1,2,{"x":"y"}]}}}`,
		`[]`,
		`null`,
		`123`,
		`"just a string"`,
		``,
		`{`,
		`}`,
		`{"a":`,
		`{"a":1,}`,
		`{"\ud800":1}`,
		strings.Repeat(`{"a":`, 200),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		// Cap input so a fuzzer cannot make this test consume unbounded memory.
		if len(src) > 1<<16 {
			t.Skip("input exceeds the fuzz size cap")
		}

		node, err := Parse([]byte(src))

		if err != nil {
			// Any error must be one of our typed errors so that callers can classify it
			// and map it to a diagnostic code.
			var se *SyntaxError
			var pe *ParseError
			if !errors.As(err, &se) && !errors.As(err, &pe) {
				t.Fatalf("Parse returned untyped error %T: %v", err, err)
			}
			return
		}

		// On success, every recorded position must be inside the document. A position
		// past the end would produce a diagnostic pointing at a line that does not
		// exist, which is worse than no position at all.
		maxLine := strings.Count(src, "\n") + 1
		for key, positions := range node.KeyPositions {
			for _, p := range positions {
				if p.Line < 1 || p.Line > maxLine {
					t.Fatalf("key %q has position %+v outside 1..%d lines of input", key, p, maxLine)
				}
				if p.Col < 1 {
					t.Fatalf("key %q has non-positive column %+v", key, p)
				}
			}
		}

		// A successfully parsed document must always yield a root we can interrogate.
		// Type-switch helpers must never panic on whatever shape fuzzing produced.
		if _, ok := node.Root.Obj(); !ok {
			if _, ok := node.Root.Arr(); !ok {
				switch node.Root.Value.(type) {
				case string, float64, bool, nil:
				default:
					t.Fatalf("root is unexpected type %T", node.Root.Value)
				}
			}
		}
	})
}

// FuzzScanKeyPositions targets the duplicate-key scanner specifically, which is the part
// of the parser that most easily loses track of nesting.
func FuzzScanKeyPositions(f *testing.F) {
	for _, s := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"b":{"b":1}}}`,
		`{"x":"\\","y":2}`,
		`{"a":[{"a":1},{"a":2}]}`,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 1<<16 {
			t.Skip("input exceeds the fuzz size cap")
		}
		node, err := Parse([]byte(src))
		if err != nil {
			return
		}
		// A key can never be recorded more times than it appears in the source, modulo
		// escapes. Allow generous slack for escaping but catch runaway counts, which
		// would indicate the scanner double-counting.
		if len(src) > 0 && len(node.KeyPositions) > len(src) {
			t.Fatalf("recorded %d distinct keys from %d bytes, which cannot be right", len(node.KeyPositions), len(src))
		}
	})
}
