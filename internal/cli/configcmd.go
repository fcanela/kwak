package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/fcanela/kwak/internal/wire"
)

const configUsage = "usage: kwak config resume | resume-exe [--harness claude|codex] [path] | " +
	"resume-per-hour <n> | check-outgoing <path|off> | check-incoming <path|off>"

// Config is kwak config resume | resume-exe [--harness claude|codex] [path] | resume-per-hour <n> |
// check-outgoing <path|off> | check-incoming <path|off>.
func (c *CLI) Config(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, configUsage)
		return 2
	}
	switch args[0] {
	case "resume":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: kwak config resume")
			return 2
		}
		return c.configResume(stdout)
	case "resume-exe":
		return c.configResumeExe(args[1:], stdout, stderr)
	case "resume-per-hour":
		return c.configResumePerHour(args[1:], stdout, stderr)
	case "check-outgoing", "check-incoming":
		return c.configCheck(args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "kwak config: unknown setting %q\n", args[0])
	return 2
}

func (c *CLI) configResume(stdout io.Writer) int {
	bot := "@registrar"
	if dir, err := c.env.ConfigDir(); err == nil {
		if cfg, err := loadConfig(dir, c.env.InsecureHTTPForTests); err == nil {
			bot += ":" + cfg.ServerName
		}
	}
	fmt.Fprintf(stdout, "Background resume is an owner setting. Only you can change it, in your chat with %s. Send one of:\n\n"+
		"    set resume off\n    set resume replies\n    set resume all\n\n"+
		"off: never resume a session in the background (the default).\n"+
		"replies: resume an ended session when someone replies in a thread it started (recommended).\n"+
		"all: also resume it when someone mentions it.\n\n"+
		"It takes effect on your machines within an hour. Only Claude Code sessions are resumed for now.\n"+
		"A resumed run needs the path of claude: run kwak config resume-exe in your own terminal.\n", bot)
	return 0
}

func (c *CLI) configResumeExe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kwak config resume-exe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", string(wire.HarnessClaude), "claude or codex")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: kwak config resume-exe [--harness claude|codex] [path]")
		return 2
	}
	h := wire.Harness(*harness)
	if !wire.ValidHarness(h) {
		return fail(stderr, "unknown harness %q", *harness)
	}
	dir, err := c.env.ConfigDir()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	cfg, err := loadConfig(dir, c.env.InsecureHTTPForTests)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	p := fs.Arg(0)
	if p == "" {
		if p, err = c.env.LookPath(string(h)); err != nil {
			return fail(stderr, "%s is not on your PATH (%v). Pass its path: kwak config resume-exe --harness %s <path>", h, err, h)
		}
	}
	if p, err = filepath.Abs(p); err != nil {
		return fail(stderr, "%v", err)
	}
	fi, err := os.Stat(p)
	if err == nil && !runnable(fi, p, runtime.GOOS == "windows") {
		err = errors.New("not an executable file")
	}
	if err != nil {
		return fail(stderr, "%s: %v", p, err)
	}
	if cfg.ResumeExe == nil {
		cfg.ResumeExe = map[wire.Harness]string{}
	}
	cfg.ResumeExe[h] = p
	if err := cfg.Save(dir); err != nil {
		return fail(stderr, "could not write %s: %v", ConfigFile, err)
	}
	fmt.Fprintf(stdout, "Recorded %s for background resume of %s sessions. The listener reads it when it starts; kwak install restarts it.\n", p, h)
	return 0
}

// configResumePerHour records the cap on background resume runs started on this machine in any 60
// minutes (decision 38, spec 5.11). Without it the listener uses wire.DefaultResumePerHour.
func (c *CLI) configResumePerHour(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: kwak config resume-per-hour <n>"
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 {
		fmt.Fprintf(stderr, "kwak config resume-per-hour: %q is not a whole number of 1 or more (the default is %d)\n%s\n",
			args[0], wire.DefaultResumePerHour, usage)
		return 2
	}
	dir, err := c.env.ConfigDir()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	cfg, err := loadConfig(dir, c.env.InsecureHTTPForTests)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	cfg.ResumePerHour = n
	if err := cfg.Save(dir); err != nil {
		return fail(stderr, "could not write %s: %v", ConfigFile, err)
	}
	fmt.Fprintf(stdout, "Recorded %d background resume runs per hour on this machine. More runs wait until the hour allows them; "+
		"none is dropped. The listener reads it when it starts; kwak install restarts it.\n", n)
	return 0
}

// configCheck records the outgoing or the incoming check command (spec 5.16), or turns it off.
// The path must be absolute: a check is a guard, so kwak never resolves it against a directory or
// PATH.
func (c *CLI) configCheck(which string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: kwak config %s <path|off>\n", which)
		return 2
	}
	dir, err := c.env.ConfigDir()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	cfg, err := loadConfig(dir, c.env.InsecureHTTPForTests)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	p := args[0]
	if p != "off" {
		if !filepath.IsAbs(p) {
			return fail(stderr, "%s is not an absolute path. Give the full path of the check command, or off", p)
		}
		fi, err := os.Stat(p)
		if err == nil && !runnable(fi, p, runtime.GOOS == "windows") {
			err = errors.New("not an executable file")
		}
		if err != nil {
			return fail(stderr, "%s: %v", p, err)
		}
	}
	slot, what := &cfg.CheckOutgoing, "outgoing"
	if which == "check-incoming" {
		slot, what = &cfg.CheckIncoming, "incoming"
	}
	const reader = "The listener reads it for each message, so no restart is needed."
	if p == "off" {
		*slot = ""
	} else {
		*slot = p
	}
	if err := cfg.Save(dir); err != nil {
		return fail(stderr, "could not write %s: %v", ConfigFile, err)
	}
	if p == "off" {
		fmt.Fprintf(stdout, "The %s check command is off. The secrets scan still runs. %s\n", what, reader)
		return 0
	}
	fmt.Fprintf(stdout, "Recorded %s as the %s check command. %s\n", p, what, reader)
	return 0
}

// runnable reports whether fi at path p can be started: an exec bit, or on Windows (no exec bit)
// an extension Windows runs.
func runnable(fi os.FileInfo, p string, windows bool) bool {
	if !fi.Mode().IsRegular() {
		return false
	}
	if !windows {
		return fi.Mode()&0o111 != 0
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".exe", ".cmd", ".bat", ".com":
		return true
	}
	return false
}
