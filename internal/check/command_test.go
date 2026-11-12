package check

import (
	"context"
	"strings"
	"testing"
)

// recRunner is a Runner double that records its calls.
type recRunner struct {
	calls int
	exit  int
	out   string
	err   error
}

func (r *recRunner) Run(context.Context, string, []byte) (int, []byte, error) {
	r.calls++
	return r.exit, []byte(r.out), r.err
}

func TestCommandNeverRunsARelativePath(t *testing.T) {
	r := &recRunner{}
	v := Command(context.Background(), r, "bin/check", "hello")
	if v.OK || r.calls != 0 || !strings.Contains(v.Reason, "the check failed") {
		t.Fatalf("verdict %+v after %d runs", v, r.calls)
	}
}

func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "",
		"  too vague  \nsecond line": "too vague",
		"tab\there\x1b[31m":          "tab here [31m",
		strings.Repeat("é", 250):     strings.Repeat("é", MaxReason) + "...",
		"\u009b31mred":               "31mred",
		"left\u202Eright\u200b":      "leftright",
	} {
		if got := FirstLine([]byte(in)); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
