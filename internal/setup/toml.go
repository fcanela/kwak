package setup

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// tomlEntry is one table header or key of a TOML file, with its full key path. This is a small
// line scanner, enough to find kwak's table and Codex's hooks.state keys; it is not a TOML parser.
type tomlEntry struct {
	Path   []string
	Value  string // the raw text after "=", trimmed; empty for a header
	Header bool
	Array  bool // a [[header]]
	Line   int  // 0-based
}

func scanTOML(s string) []tomlEntry {
	var out []tomlEntry
	var table []string
	ls := strings.Split(s, "\n")
	for i := 0; i < len(ls); i++ {
		l := strings.TrimSpace(strings.TrimRight(ls[i], "\r"))
		if l == "" || l[0] == '#' {
			continue
		}
		if strings.HasPrefix(l, "[[") {
			if segs, rest, ok := keyPath(l[2:]); ok && strings.HasPrefix(strings.TrimSpace(rest), "]]") {
				table = segs
				out = append(out, tomlEntry{Path: segs, Header: true, Array: true, Line: i})
			}
			continue
		}
		if l[0] == '[' {
			if segs, rest, ok := keyPath(l[1:]); ok && strings.HasPrefix(strings.TrimSpace(rest), "]") {
				table = segs
				out = append(out, tomlEntry{Path: segs, Header: true, Line: i})
			}
			continue
		}
		segs, rest, ok := keyPath(l)
		rest = strings.TrimSpace(rest)
		if !ok || !strings.HasPrefix(rest, "=") {
			continue
		}
		val := strings.TrimSpace(rest[1:])
		full := append(append([]string(nil), table...), segs...)
		out = append(out, tomlEntry{Path: full, Value: val, Line: i})
		i = skipMultiline(ls, i, val)
	}
	return out
}

// skipMultiline returns the last line of a value that starts on line i and spans lines: a
// multi-line string or an array.
func skipMultiline(ls []string, i int, val string) int {
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(val, q) {
			if strings.Contains(val[3:], q) {
				return i
			}
			for j := i + 1; j < len(ls); j++ {
				if strings.Contains(ls[j], q) {
					return j
				}
			}
			return len(ls)
		}
	}
	if strings.HasPrefix(val, "[") || strings.HasPrefix(val, "{") {
		depth := bracketDepth(val)
		j := i
		for depth > 0 && j+1 < len(ls) {
			j++
			depth += bracketDepth(ls[j])
		}
		return j
	}
	return i
}

func bracketDepth(s string) int {
	d := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return d
		case c == '[' || c == '{':
			d++
		case c == ']' || c == '}':
			d--
		}
	}
	return d
}

// keyPath reads a dotted key at the start of s and returns its segments and the rest.
func keyPath(s string) ([]string, string, bool) {
	var segs []string
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return nil, "", false
		}
		var seg string
		switch s[0] {
		case '"':
			end := 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(s) {
				return nil, "", false
			}
			seg = unescape(s[1:end])
			s = s[end+1:]
		case '\'':
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, "", false
			}
			seg = s[1 : end+1]
			s = s[end+2:]
		default:
			n := 0
			for n < len(s) && (isBare(s[n])) {
				n++
			}
			if n == 0 {
				return nil, "", false
			}
			seg, s = s[:n], s[n:]
		}
		segs = append(segs, seg)
		s = strings.TrimLeft(s, " \t")
		if !strings.HasPrefix(s, ".") {
			return segs, s, true
		}
		s = s[1:]
	}
}

func isBare(c byte) bool {
	return c == '_' || c == '-' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'u', 'U':
			n := 4
			if s[i] == 'U' {
				n = 8
			}
			if i+1+n <= len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += n
					continue
				}
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// hasKwakServer reports whether the file defines mcp_servers.kwak in any form.
func hasKwakServer(entries []tomlEntry) bool {
	for _, e := range entries {
		if len(e.Path) >= 2 && e.Path[0] == "mcp_servers" && e.Path[1] == "kwak" {
			return true
		}
		if isMCPServersValue(e) && strings.HasPrefix(e.Value, "{") && slices.Contains(inlineKeys(e.Value), "kwak") {
			return true
		}
	}
	return false
}

// isMCPServersValue reports whether e gives mcp_servers itself a value (an inline table, say),
// which a [mcp_servers.kwak] table cannot extend.
func isMCPServersValue(e tomlEntry) bool {
	return len(e.Path) == 1 && e.Path[0] == "mcp_servers" && (!e.Header || e.Array)
}

// inlineKeys returns the first key segment of each top-level entry of an inline table.
func inlineKeys(val string) []string {
	var keys []string
	depth := 0
	var quote byte
	for i := 0; i < len(val); i++ {
		c := val[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		}
		if depth == 1 && (c == '{' || c == ',') {
			if segs, _, ok := keyPath(val[i+1:]); ok {
				keys = append(keys, segs[0])
			}
		}
	}
	return keys
}

var inlineTrusted = regexp.MustCompile(`(?:^|[{,])\s*(?:trusted_hash|"trusted_hash"|'trusted_hash')\s*=\s*["']`)

// trustedHooks returns the hooks.state keys that carry a trusted_hash.
func trustedHooks(entries []tomlEntry) map[string]bool {
	keys := map[string]bool{}
	for _, e := range entries {
		p := e.Path
		if len(p) < 3 || p[0] != "hooks" || p[1] != "state" || e.Header {
			continue
		}
		switch {
		case len(p) == 4 && p[3] == "trusted_hash" && (strings.HasPrefix(e.Value, `"`) || strings.HasPrefix(e.Value, "'")):
			keys[p[2]] = true
		case len(p) == 3 && strings.HasPrefix(e.Value, "{") && inlineTrusted.MatchString(e.Value):
			keys[p[2]] = true
		}
	}
	return keys
}
