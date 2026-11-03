package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Config block markers in config.toml.
const (
	markerBegin = "# >>> kwak (written by kwak setup codex)"
	markerEnd   = "# <<< kwak"
)

// HooksDescription is the description of a hooks.json that kwak creates.
const HooksDescription = "kwak: messages from other agents"

type codexEvent struct {
	Name    string // Codex's event key in hooks.json
	Event   string // kwak's event name
	Label   string // Codex's hooks.state key label
	Matcher string
	Timeout int
}

var codexEvents = []codexEvent{
	{"SessionStart", "session-start", "session_start", "", 10},
	{"UserPromptSubmit", "prompt", "user_prompt_submit", "", 10},
	{"PostToolUse", "post-tool", "post_tool_use", "*", 10},
	{"Stop", "stop", "stop", "", 10},
	{"SubagentStop", "subagent-stop", "subagent_stop", "", 10},
	{"SessionEnd", "session-end", "session_end", "", 3},
}

type handler struct {
	Type           string `json:"type"`
	Command        string `json:"command"`
	CommandWindows string `json:"commandWindows,omitempty"`
	Timeout        int    `json:"timeout"`
}

type group struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []handler `json:"hooks"`
}

// posixQuote quotes exe for $SHELL -c when it holds anything but safe characters.
func posixQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_./:@%+=,-", r))) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// windowsBare matches the paths PowerShell and cmd both read as one plain word.
var windowsBare = regexp.MustCompile(`^[A-Za-z]:\\[A-Za-z0-9_.\\-]+$`)

// windowsCommand is the hook line for PowerShell, and for cmd when exe is a plain path (spec 6.3).
// Any other path is a PowerShell literal string, where only ' is special and is doubled.
func windowsCommand(exe, event string) string {
	if windowsBare.MatchString(exe) {
		return exe + " hook codex " + event
	}
	return `& '` + strings.ReplaceAll(exe, "'", "''") + `' hook codex ` + event
}

func codexGroup(exe, goos string, e codexEvent) group {
	h := handler{Type: "command", Command: posixQuote(exe) + " hook codex " + e.Event, Timeout: e.Timeout}
	if goos == "windows" {
		h.Command = windowsCommand(exe, e.Event)
		h.CommandWindows = h.Command
	}
	return group{Matcher: e.Matcher, Hooks: []handler{h}}
}

// isKwakCommand reports whether cmd is a kwak hook line for event, in any form kwak writes.
func isKwakCommand(cmd, event string) bool {
	prog, ok := strings.CutSuffix(cmd, " hook codex "+event)
	if !ok {
		return false
	}
	prog, ps := strings.CutPrefix(prog, "& ")
	switch {
	case len(prog) >= 2 && prog[0] == '\'' && prog[len(prog)-1] == '\'' && ps:
		prog = strings.ReplaceAll(prog[1:len(prog)-1], "''", "'")
	case len(prog) >= 2 && prog[0] == '\'' && prog[len(prog)-1] == '\'':
		prog = strings.ReplaceAll(prog[1:len(prog)-1], `'\''`, "'")
	case len(prog) >= 2 && prog[0] == '"' && prog[len(prog)-1] == '"':
		prog = prog[1 : len(prog)-1]
	}
	if i := strings.LastIndexAny(prog, `/\`); i >= 0 {
		prog = prog[i+1:]
	}
	prog = strings.ToLower(prog)
	return prog == "kwak" || prog == "kwak.exe"
}

// isKwakGroup reports whether every hook of a hooks.json group is a kwak hook for event.
func isKwakGroup(raw json.RawMessage, event string) bool {
	var g struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if json.Unmarshal(raw, &g) != nil || len(g.Hooks) == 0 {
		return false
	}
	for _, h := range g.Hooks {
		if !isKwakCommand(h.Command, event) {
			return false
		}
	}
	return true
}

// object is a JSON object that keeps its key order and its values verbatim.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

func parseObject(b []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	o := newObject()
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(t.(string), v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON object")
	}
	return o, nil
}

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, kk := range o.keys {
		if kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshal(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func indent(v any) []byte {
	var out bytes.Buffer
	json.Indent(&out, marshal(v), "", "  ")
	out.WriteByte('\n')
	return out.Bytes()
}

// hooksDoc is a whole hooks.json with kwak's hooks only.
func hooksDoc(exe, goos string) []byte {
	hooks := newObject()
	for _, e := range codexEvents {
		hooks.set(e.Name, marshal([]group{codexGroup(exe, goos, e)}))
	}
	doc := newObject()
	doc.set("description", marshal(HooksDescription))
	doc.set("hooks", marshal(hooks))
	return indent(doc)
}

// hookChanges says what a merge or removal did, by Codex event name.
type hookChanges struct {
	Added, Changed, Removed []string
}

func (c hookChanges) any() bool { return len(c.Added)+len(c.Changed)+len(c.Removed) > 0 }

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func loadHooks(existing []byte) (doc, hooks *object, err error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		doc, hooks = newObject(), newObject()
		doc.set("description", marshal(HooksDescription))
		return doc, hooks, nil
	}
	if doc, err = parseObject(existing); err != nil {
		return nil, nil, fmt.Errorf("hooks.json is not valid: %v", err)
	}
	hooks = newObject()
	if raw, ok := doc.vals["hooks"]; ok && string(raw) != "null" {
		if hooks, err = parseObject(raw); err != nil {
			return nil, nil, fmt.Errorf("hooks.json: \"hooks\" is not an object: %v", err)
		}
	}
	return doc, hooks, nil
}

func groupsOf(hooks *object, name string) ([]json.RawMessage, error) {
	var groups []json.RawMessage
	if raw, ok := hooks.vals[name]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, fmt.Errorf("hooks.json: %s is not a list: %v", name, err)
		}
	}
	return groups, nil
}

// mergeHooks adds kwak's hook to each event, replaces a kwak hook that changed in place (so its
// position, and the trust of the others, stays), and leaves every other hook alone.
func mergeHooks(existing []byte, exe, goos string) ([]byte, hookChanges, error) {
	var ch hookChanges
	doc, hooks, err := loadHooks(existing)
	if err != nil {
		return nil, ch, err
	}
	for _, e := range codexEvents {
		groups, err := groupsOf(hooks, e.Name)
		if err != nil {
			return nil, ch, err
		}
		want := marshal(codexGroup(exe, goos, e))
		var out []json.RawMessage
		found := false
		for _, g := range groups {
			if !isKwakGroup(g, e.Event) {
				out = append(out, g)
				continue
			}
			if found {
				ch.Changed = appendOnce(ch.Changed, e.Name)
				continue
			}
			found = true
			if !sameJSON(g, want) {
				ch.Changed = appendOnce(ch.Changed, e.Name)
				g = want
			}
			out = append(out, g)
		}
		if !found {
			out = append(out, want)
			ch.Added = append(ch.Added, e.Name)
		}
		if found && !contains(ch.Changed, e.Name) {
			continue
		}
		hooks.set(e.Name, marshal(out))
	}
	if !ch.any() {
		return existing, ch, nil
	}
	doc.set("hooks", marshal(hooks))
	return indent(doc), ch, nil
}

// removeHooks drops kwak's hooks. It returns nil when the file held nothing else and kwak
// made it.
func removeHooks(existing []byte) ([]byte, hookChanges, error) {
	var ch hookChanges
	if len(bytes.TrimSpace(existing)) == 0 {
		return existing, ch, nil
	}
	doc, hooks, err := loadHooks(existing)
	if err != nil {
		return nil, ch, err
	}
	for _, e := range codexEvents {
		groups, err := groupsOf(hooks, e.Name)
		if err != nil {
			return nil, ch, err
		}
		var out []json.RawMessage
		for _, g := range groups {
			if isKwakGroup(g, e.Event) {
				ch.Removed = appendOnce(ch.Removed, e.Name)
			} else {
				out = append(out, g)
			}
		}
		if !contains(ch.Removed, e.Name) {
			continue
		}
		if len(out) == 0 {
			hooks.del(e.Name)
		} else {
			hooks.set(e.Name, marshal(out))
		}
	}
	if !ch.any() {
		return existing, ch, nil
	}
	if len(hooks.keys) == 0 && onlyOurs(doc) {
		return nil, ch, nil
	}
	doc.set("hooks", marshal(hooks))
	return indent(doc), ch, nil
}

// onlyOurs reports whether doc holds nothing but hooks and kwak's description.
func onlyOurs(doc *object) bool {
	for _, k := range doc.keys {
		switch {
		case k == "hooks":
		case k == "description" && sameJSON(doc.vals[k], marshal(HooksDescription)):
		default:
			return false
		}
	}
	return true
}

// kwakHookKeys gives the hooks.state key of each kwak hook in a hooks.json, by Codex event name
// (Codex's hook_key: "<file>:<label>:<group>:<handler>").
func kwakHookKeys(existing []byte, file string) map[string][]string {
	keys := map[string][]string{}
	_, hooks, err := loadHooks(existing)
	if err != nil {
		return keys
	}
	for _, e := range codexEvents {
		groups, _ := groupsOf(hooks, e.Name)
		for gi, g := range groups {
			if !isKwakGroup(g, e.Event) {
				continue
			}
			var n struct{ Hooks []json.RawMessage }
			json.Unmarshal(g, &n)
			for hi := range n.Hooks {
				keys[e.Name] = append(keys[e.Name], file+":"+e.Label+":"+strconv.Itoa(gi)+":"+strconv.Itoa(hi))
			}
		}
	}
	return keys
}

func appendOnce(s []string, v string) []string {
	if contains(s, v) {
		return s
	}
	return append(s, v)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// tomlString is s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// configBlock is kwak's config.toml block (spec 6.3).
func configBlock(exe string) string {
	return markerBegin + "\n" +
		"[mcp_servers.kwak]\n" +
		"command = " + tomlString(exe) + "\n" +
		`args = ["mcp", "--harness", "codex"]` + "\n" +
		"startup_timeout_sec = 10\n" +
		"tool_timeout_sec = 30\n" +
		`default_tools_approval_mode = "approve"` + "\n" +
		markerEnd + "\n"
}

// configResult says what a config.toml merge or removal did.
type configResult int

const (
	configUnchanged configResult = iota
	configAdded
	configChanged
	configRemoved
	configForeign // [mcp_servers.kwak] exists outside kwak's markers
)

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(s, "\n")
}

func trimLine(l string) string { return strings.TrimSpace(strings.TrimRight(l, "\r\n")) }

// findBlock returns the line range [begin, end] of kwak's block, or -1.
func findBlock(ls []string) (begin, end int, err error) {
	begin, end = -1, -1
	for i, l := range ls {
		t := trimLine(l)
		if begin < 0 && strings.HasPrefix(t, "# >>> kwak") {
			begin = i
		} else if begin >= 0 && t == markerEnd {
			return begin, i, nil
		}
	}
	if begin >= 0 {
		return -1, -1, errors.New("config.toml has kwak's start marker but no end marker; fix it by hand")
	}
	return -1, -1, nil
}

// kwakSpan returns where kwak's table ends inside its block [begin, end]: at the next table
// header, not at the end marker. The block must start with kwak's table. Codex adds a table (a hooks.state trust entry, say) after the
// last table and before an end-of-file comment, so it lands inside the block.
func kwakSpan(entries []tomlEntry, ls []string, begin, end int) (int, error) {
	start := -1
	for _, e := range entries {
		if !e.Header || e.Line <= begin || e.Line >= end {
			continue
		}
		if start < 0 {
			if !isKwakTable(e) || !allDecor(ls[begin+1:e.Line]) {
				break
			}
			start = e.Line
			continue
		}
		to := e.Line
		for to > start+1 && isDecor(ls[to-1]) {
			to--
		}
		return to, nil
	}
	if start < 0 {
		return 0, errors.New("kwak's block in config.toml does not start with [mcp_servers.kwak]; fix it by hand")
	}
	to := end
	for to > start+1 && isDecor(ls[to-1]) {
		to--
	}
	return to, nil
}

func isKwakTable(e tomlEntry) bool {
	return e.Header && !e.Array && len(e.Path) == 2 && e.Path[0] == "mcp_servers" && e.Path[1] == "kwak"
}

func isDecor(l string) bool {
	t := trimLine(l)
	return t == "" || t[0] == '#'
}

func inlineServer(exe string) string {
	return "kwak = { command = " + tomlString(exe) + `, args = ["mcp", "--harness", "codex"], ` +
		`startup_timeout_sec = 10, tool_timeout_sec = 30, default_tools_approval_mode = "approve" }`
}

func mergeConfig(existing, exe string) (string, configResult, error) {
	block := configBlock(exe)
	ls := lines(existing)
	begin, end, err := findBlock(ls)
	if err != nil {
		return "", 0, err
	}
	entries := scanTOML(existing)
	if begin >= 0 {
		to, err := kwakSpan(entries, ls, begin, end)
		if err != nil {
			return "", 0, err
		}
		own := strings.TrimSuffix(block, markerEnd+"\n")
		if strings.TrimRight(strings.Join(ls[begin:to], ""), "\r\n") == strings.TrimRight(own, "\n") {
			return existing, configUnchanged, nil
		}
		return strings.Join(ls[:begin], "") + own + strings.Join(ls[to:], ""), configChanged, nil
	}
	if hasKwakServer(entries) {
		return existing, configForeign, nil
	}
	for _, e := range entries {
		if isMCPServersValue(e) {
			return "", 0, fmt.Errorf("mcp_servers is set on line %d as a value, not as [mcp_servers.<name>] tables, so kwak cannot add its table. "+
				"Add this entry to it by hand, then run kwak setup codex --write again:\n%s", e.Line+1, inlineServer(exe))
		}
	}
	out := existing
	if out != "" {
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "\n"
	}
	return out + block, configAdded, nil
}

// removeConfig removes kwak's block. It keeps the tables Codex added inside the block, except
// the ones under mcp_servers.kwak.
func removeConfig(existing string) (string, configResult, error) {
	ls := lines(existing)
	begin, end, err := findBlock(ls)
	if err != nil {
		return "", 0, err
	}
	entries := scanTOML(existing)
	if begin < 0 {
		if hasKwakServer(entries) {
			return existing, configForeign, nil
		}
		return existing, configUnchanged, nil
	}
	to, err := kwakSpan(entries, ls, begin, end)
	if err != nil {
		return "", 0, err
	}
	var kept []string
	drop := false
	for i := to; i < end; i++ {
		for _, e := range entries {
			if e.Header && e.Line >= i && e.Line < end {
				if e.Line == i || allDecor(ls[i:e.Line]) {
					drop = len(e.Path) > 2 && e.Path[0] == "mcp_servers" && e.Path[1] == "kwak"
				}
				break
			}
		}
		if !drop {
			kept = append(kept, ls[i])
		}
	}
	if begin > 0 && trimLine(ls[begin-1]) == "" {
		begin--
	}
	return strings.Join(ls[:begin], "") + strings.Join(kept, "") + strings.Join(ls[end+1:], ""), configRemoved, nil
}

func allDecor(ls []string) bool {
	for _, l := range ls {
		if !isDecor(l) {
			return false
		}
	}
	return true
}
