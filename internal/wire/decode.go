package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Decoding errors.
var (
	ErrVersion    = errors.New("wire: protocol version newer than this kwak")
	ErrBadMessage = errors.New("wire: bad message")
)

// ReadHead decodes the shared part of an IPC request, ignoring every other field. A "v" above
// ProtocolVersion gives ErrVersion (with the head still filled in); invalid JSON or a missing "v"
// gives ErrBadMessage. The listener answers both with Refuse.
func ReadHead(line []byte) (Head, error) {
	var h Head
	if err := json.Unmarshal(line, &h); err != nil {
		return Head{}, fmt.Errorf("%w: %v", ErrBadMessage, err)
	}
	if h.V > ProtocolVersion {
		return h, ErrVersion
	}
	if h.V < 1 {
		return h, fmt.Errorf("%w: missing v", ErrBadMessage)
	}
	return h, nil
}

// Decode decodes one IPC line into v. It is tolerant: unknown fields are ignored (spec 5.6).
func Decode(line []byte, v any) error {
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadMessage, err)
	}
	return nil
}

// Refuse is the reply to a request refused before its op ran, for example IPCErrVersion.
func Refuse(code string) Ack {
	return Ack{V: ProtocolVersion, OK: false, Err: code}
}

// DecodeAPI decodes a registrar API body strictly (spec 3.4): at most MaxBody bytes, one JSON
// value, no unknown fields. The registrar answers any failure with 400 ErrBadRequest.
func DecodeAPI(body []byte, v any) error {
	if len(body) > MaxBody {
		return fmt.Errorf("%w: body over %d bytes", ErrBadMessage, MaxBody)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadMessage, err)
	}
	if !atEOF(dec) {
		return fmt.Errorf("%w: trailing data", ErrBadMessage)
	}
	return nil
}

// atEOF reports whether only whitespace follows the value dec just decoded.
func atEOF(dec *json.Decoder) bool {
	_, err := dec.Token()
	return err == io.EOF
}

// CompareVersions compares two "major.minor.patch" versions and returns -1, 0 or 1.
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}

func parseVersion(s string) ([3]int, error) {
	var v [3]int
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("wire: bad version %q", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return v, fmt.Errorf("wire: bad version %q", s)
		}
		v[i] = n
	}
	return v, nil
}

// ValidHarness reports whether h is a supported harness.
func ValidHarness(h Harness) bool { return h == HarnessClaude || h == HarnessCodex }

// ValidEvent reports whether e is a kwak hook event.
func ValidEvent(e string) bool {
	switch e {
	case EventSessionStart, EventPrompt, EventPostTool, EventStop, EventSubagentStop, EventSessionEnd:
		return true
	}
	return false
}

// hookStdin holds the hook stdin fields kwak reads, each kept raw so that a field of the wrong
// type reads as absent instead of failing the hook (spec 5.6).
type hookStdin struct {
	SessionID      json.RawMessage `json:"session_id"`
	AgentID        json.RawMessage `json:"agent_id"`
	AgentType      json.RawMessage `json:"agent_type"`
	Source         json.RawMessage `json:"source"`
	Cwd            json.RawMessage `json:"cwd"`
	StopHookActive json.RawMessage `json:"stop_hook_active"`
	ToolName       json.RawMessage `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	HookEventName  json.RawMessage `json:"hook_event_name"`
}

// field decodes raw into v and leaves v zero when raw is absent or of another type.
func field[T any](raw json.RawMessage) T {
	var v T
	if len(raw) > 0 && json.Unmarshal(raw, &v) != nil {
		var zero T
		return zero
	}
	return v
}

// ParseHookStdin builds the HookRequest for one hook call from the harness's stdin. Decoding is
// tolerant: unknown fields are ignored, and a known field of the wrong type counts as absent.
// stdin must still be one JSON object or null. tool_input is kept only when it is an object and the tool
// is one of kwak's own. It also returns hook_event_name, which Claude's output echoes. Chain is
// left for the caller (Windows).
func ParseHookStdin(h Harness, event string, stdin []byte) (HookRequest, string, error) {
	if !ValidHarness(h) {
		return HookRequest{}, "", fmt.Errorf("wire: unknown harness %q", h)
	}
	if !ValidEvent(event) {
		return HookRequest{}, "", fmt.Errorf("wire: unknown hook event %q", event)
	}
	var in hookStdin
	if err := Decode(stdin, &in); err != nil {
		return HookRequest{}, "", err
	}
	r := HookRequest{
		V: ProtocolVersion, Kwak: Version, Op: OpHook, Harness: h, Event: event,
		SessionID:      field[string](in.SessionID),
		AgentID:        field[string](in.AgentID),
		AgentType:      field[string](in.AgentType),
		Source:         field[string](in.Source),
		Cwd:            field[string](in.Cwd),
		StopHookActive: field[bool](in.StopHookActive),
		ToolName:       field[string](in.ToolName),
	}
	if _, ok := ToolFromName(h, r.ToolName); ok && field[map[string]json.RawMessage](in.ToolInput) != nil {
		r.ToolInput = in.ToolInput
	}
	return r, field[string](in.HookEventName), nil
}

// MCP tool names, as the MCP server lists them and the tool op carries them.
const (
	ToolSend   = "send"
	ToolReply  = "reply"
	ToolThread = "thread"
	ToolHelper = "helper"
	ToolStatus = "status"
)

// Tools lists every MCP tool name.
var Tools = []string{ToolSend, ToolReply, ToolThread, ToolHelper, ToolStatus}

// ToolPrefix is the prefix a harness puts before kwak's tool names.
func ToolPrefix(h Harness) string {
	switch h {
	case HarnessClaude:
		return "mcp__plugin_kwak_kwak__"
	case HarnessCodex:
		return "mcp__kwak__"
	}
	return ""
}

// ToolFromName returns the kwak tool behind a harness tool name, such as
// "mcp__plugin_kwak_kwak__helper" on Claude.
func ToolFromName(h Harness, name string) (string, bool) {
	p := ToolPrefix(h)
	if p == "" {
		return "", false
	}
	t, ok := strings.CutPrefix(name, p)
	if !ok {
		return "", false
	}
	for _, k := range Tools {
		if t == k {
			return t, true
		}
	}
	return "", false
}
