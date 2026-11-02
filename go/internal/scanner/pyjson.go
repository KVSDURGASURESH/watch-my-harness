package scanner

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The tool_calls_json column is read back verbatim by /api/sessions/<id>, so it has
// to be byte-identical to Python's json.dumps (", " / ": " separators, ensure_ascii).

func pyQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r < 0x7f || r == 0x7f:
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyDumps serialises a decoded-JSON value like json.dumps (map keys sorted: Go maps are unordered).
func pyDumps(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return pyQuote(x)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e16 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyDumps(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyQuote(k) + ": " + pyDumps(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyQuote(fmt.Sprint(v))
}

type call struct {
	Name   string
	Target *string
}

// dumpCalls == json.dumps([{"name": n, "target": t}, ...])
func dumpCalls(cs []call) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		t := "null"
		if c.Target != nil {
			t = pyQuote(*c.Target)
		}
		parts[i] = `{"name": ` + pyQuote(c.Name) + `, "target": ` + t + `}`
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ---- dynamic-typing helpers (Python truthiness / int() / slicing semantics)

type obj = map[string]any

func asObj(v any) obj { o, _ := v.(map[string]any); return o }
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
func asStr(v any) (string, bool) { s, ok := v.(string); return s, ok }

// truthy follows Python: None, False, 0, "", [], {} are falsy.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// toInt is int(v or 0).
func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// trunc is Python's s[:n] on code points.
func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func runeLen(s string) int64 { return int64(utf8.RuneCountInString(s)) }

func lstripSpace(s string) string { return strings.TrimLeftFunc(s, unicode.IsSpace) }
