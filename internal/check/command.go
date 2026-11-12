package check

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Runner runs a check command with stdin as its input and returns its exit code and the start of
// its stdout. Exec is the real one; tests may pass a double.
type Runner interface {
	Run(ctx context.Context, exe string, stdin []byte) (exit int, stdout []byte, err error)
}

// MaxReason caps a reason in characters, so a command cannot fill a notice or the log.
const MaxReason = 200

// maxOut caps what is read from a command's stdout.
const maxOut = 4 << 10

// Verdict is the result of the checks on one message. Kind is "secrets" or "command".
type Verdict struct {
	OK     bool
	Kind   string
	Reason string
}

// Command runs the check command exe on text (spec 5.16): exit 0 passes; any other exit refuses,
// with the first line of stdout as the reason. A command that cannot start, or that does not end
// before ctx, refuses too (fail closed), with a reason that says the check failed. A relative exe
// is never run.
func Command(ctx context.Context, r Runner, exe, text string) Verdict {
	refuse := func(reason string) Verdict { return Verdict{Kind: "command", Reason: reason} }
	if !filepath.IsAbs(exe) {
		return refuse("the check failed: its command is not an absolute path")
	}
	code, out, err := r.Run(ctx, exe, []byte(text))
	switch {
	case ctx.Err() != nil:
		return refuse("the check failed: it did not answer in time")
	case err != nil:
		return refuse("the check failed: its command could not run")
	case code == 0:
		return Verdict{OK: true, Kind: "command"}
	}
	reason := FirstLine(out)
	if reason == "" {
		reason = "the check gave no reason"
	}
	return refuse(reason)
}

// FirstLine is the first line of out, trimmed, without control characters (C0 and C1) or
// invisible format characters (such as bidi overrides), and cut to MaxReason characters. kwak
// status prints it to a terminal, and it may come from a model that read a stranger's text.
func FirstLine(out []byte) string {
	line, _, _ := bytes.Cut(out, []byte("\n"))
	s := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r) || r == utf8.RuneError:
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, string(line))
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxReason {
		s = string([]rune(s)[:MaxReason]) + "..."
	}
	return s
}

// Exec runs a check command with os/exec, in its own process group where the OS has one, so a
// timeout also stops what the command started (a model call, for example).
type Exec struct {
	// Grace is how long Run waits for stdout to close after the command has exited or been
	// killed (exec's WaitDelay), so a child that keeps stdout open never holds a check; 1 s when
	// zero. A timeout kills at once.
	Grace time.Duration
}

// Run implements Runner. A non-zero exit is not an error: it is returned as exit.
func (e Exec) Run(ctx context.Context, exe string, stdin []byte) (int, []byte, error) {
	grace := e.Grace
	if grace <= 0 {
		grace = time.Second
	}
	cmd := exec.CommandContext(ctx, exe)
	cmd.Stdin = bytes.NewReader(stdin)
	out := &capped{max: maxOut}
	cmd.Stdout = out
	cmd.WaitDelay = grace
	group(cmd)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return ee.ExitCode(), out.b.Bytes(), nil
	}
	// The command exited, but something it started kept stdout open past the grace: its own exit
	// code is the answer.
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil && cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode(), out.b.Bytes(), nil
	}
	if err != nil {
		return -1, out.b.Bytes(), err
	}
	return 0, out.b.Bytes(), nil
}

// capped keeps the first max bytes written and drops the rest, so a chatty command never blocks.
type capped struct {
	b   bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}
