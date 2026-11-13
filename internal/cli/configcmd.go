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

	"github.com/fcanela/kwak/internal/listener"
	"github.com/fcanela/kwak/internal/wire"
)

const configUsage = "usage: kwak config resume [off|<triggers>] | resume-exe [--harness claude|codex] [path] | " +
	"resume-per-hour <n> | check-outgoing <path|default|off> | check-incoming <path|default|off> | responder [on|off] | autostart [on|off]"

// Config is kwak config resume [off|<triggers>] | resume-exe [--harness claude|codex] [path] |
// resume-per-hour <n> | check-outgoing <path|default|off> | check-incoming <path|default|off> |
// responder [on|off] | autostart [on|off].
func (c *CLI) Config(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, configUsage)
		return 2
	}
	switch args[0] {
	case "resume":
		return c.configResume(args[1:], stdout, stderr)
	case "resume-exe":
		return c.configResumeExe(args[1:], stdout, stderr)
	case "resume-per-hour":
		return c.configResumePerHour(args[1:], stdout, stderr)
	case "check-outgoing", "check-incoming":
		return c.configCheck(args[0], args[1:], stdout, stderr)
	case "responder":
		return c.configResponder(args[1:], stdout, stderr)
	case "autostart":
		return c.configAutostart(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "kwak config: unknown setting %q\n", args[0])
	return 2
}

const resumeUsage = "usage: kwak config resume [off | replies,mentions | all]"

const resumeHelp = "replies: resume an ended session when someone replies in a thread it started (recommended).\n" +
	"mentions: resume it when someone mentions its agent outside that thread.\n" +
	"all: replies and mentions. off: never (the default).\n"

// errResumeOpen is the answer to the open trigger of decision 31 (an open question): the responder
// answers those, not a resumed session (decision 46, spec 5.11).
var errResumeOpen = errors.New("open questions are answered by the responder, not by a resumed session: " +
	"run kwak config responder on. Use replies, mentions or all here")

// parseResume reads the triggers of kwak config resume: off, or a comma list of replies, mentions
// and all. all is stored as replies and mentions, so the file says what fires. nil is off.
func parseResume(arg string) ([]string, error) {
	if strings.ToLower(strings.TrimSpace(arg)) == "off" {
		return nil, nil
	}
	var replies, mentions bool
	for _, w := range strings.Split(arg, ",") {
		switch strings.ToLower(strings.TrimSpace(w)) {
		case wire.ResumeReplies:
			replies = true
		case wire.ResumeMentions:
			mentions = true
		case "all":
			replies, mentions = true, true
		case "open":
			return nil, errResumeOpen
		case "off":
			return nil, errors.New("off cannot be combined with other triggers")
		default:
			return nil, fmt.Errorf("%q is not a trigger. Use off, or replies, mentions or all", w)
		}
	}
	var on []string
	if replies {
		on = append(on, wire.ResumeReplies)
	}
	if mentions {
		on = append(on, wire.ResumeMentions)
	}
	return on, nil
}

// configResume shows or sets this machine's background resume triggers (decision 44, spec 5.11).
func (c *CLI) configResume(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprintln(stderr, resumeUsage)
		return 2
	}
	var on []string
	if len(args) == 1 {
		var err error
		if on, err = parseResume(args[0]); err != nil {
			fmt.Fprintf(stderr, "kwak config resume: %v.\n%s\n", err, resumeUsage)
			return 2
		}
	}
	dir, err := c.env.ConfigDir()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	cfg, err := loadConfig(dir, c.env.InsecureHTTPForTests)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if len(args) == 0 {
		fmt.Fprintf(stdout, "Background resume on this machine: %s.\n\n%s\n%s\n", resumeText(cfg.ResumeTriggers()), resumeHelp, resumeUsage)
		return 0
	}
	cfg.Resume = on
	if err := cfg.Save(dir); err != nil {
		return fail(stderr, "could not write %s: %v", ConfigFile, err)
	}
	fmt.Fprintf(stdout, "Background resume on this machine is now %s. The listener reads it before each batch of messages, "+
		"so no restart is needed.\n", resumeText(on))
	if len(on) > 0 {
		fmt.Fprintln(stdout, "Only Claude Code sessions are resumed for now.")
		if cfg.ResumeExe[wire.HarnessClaude] == "" {
			fmt.Fprintln(stdout, "A resumed run needs the path of claude: run kwak config resume-exe in your own terminal.")
		}
	}
	return 0
}

func resumeText(on []string) string {
	if len(on) == 0 {
		return "off"
	}
	return strings.Join(on, ", ")
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
// PATH. default writes kwak's default screener (decision 46) and records it.
func (c *CLI) configCheck(which string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: kwak config %s <path|default|off>\n", which)
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
	slot, what := &cfg.CheckOutgoing, "outgoing"
	if which == "check-incoming" {
		slot, what = &cfg.CheckIncoming, "incoming"
	}
	if p == "default" {
		if p, err = writeScreener(dir, what, cfg.ResumeExe[wire.HarnessClaude], runtime.GOOS == "windows"); err != nil {
			return fail(stderr, "%v", err)
		}
	} else if p != "off" {
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
	if args[0] == "default" {
		wait := wire.CheckOutWait
		if what == "incoming" {
			wait = wire.CheckInWait
		}
		fmt.Fprintf(stdout, "It is kwak's default screener: one haiku call per message. Edit its prompt in %s.\n"+
			"A call that is slow (over %s) or fails stops the message. The script keeps the claude path it was "+
			"written with; delete it to have kwak write it again.\n", filepath.Join(filepath.Dir(p), what+".md"), wait)
	}
	return 0
}

const responderUsage = "usage: kwak config responder [on|off]"

// configResponder shows or sets this machine's responder (decision 46, spec 5.18). on also makes
// the responder folder with a default AGENTS.md when it has none.
func (c *CLI) configResponder(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 || len(args) == 1 && args[0] != "on" && args[0] != "off" {
		fmt.Fprintln(stderr, responderUsage)
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
	folder := listener.ResponderDir(&cfg.MachineConfig, filepath.Join(dir, ConfigFile))
	if len(args) == 0 {
		state := "off"
		if cfg.Responder {
			state = "on"
		}
		fmt.Fprintf(stdout, "Responder on this machine: %s. Its folder: %s.\n\n%s\n%s\n", state, folder, responderHelp, responderUsage)
		return 0
	}
	cfg.Responder = args[0] == "on"
	if cfg.Responder {
		if err := listener.EnsureResponderDir(folder); err != nil {
			return fail(stderr, "could not make the responder folder: %v", err)
		}
	}
	if err := cfg.Save(dir); err != nil {
		return fail(stderr, "could not write %s: %v", ConfigFile, err)
	}
	if !cfg.Responder {
		fmt.Fprintln(stdout, "The responder on this machine is now off. A run already going ends as usual; no new question starts one.")
		return 0
	}
	fmt.Fprintf(stdout, "The responder on this machine is now on. The listener reads it before each batch of messages, "+
		"so no restart is needed.\nIts folder is %s. Edit AGENTS.md there: where to look for answers, and what it may share.\n"+
		"Each open question from another person's agent starts one unattended, billed run that can read your files.\n", folder)
	if len(cfg.ResponderCommand) == 0 && cfg.ResumeExe[wire.HarnessClaude] == "" {
		fmt.Fprintln(stdout, "A run needs the path of claude: run kwak config resume-exe in your own terminal, then kwak install.")
	}
	return 0
}

const responderHelp = "on: answer open questions from other people's agents, one run per question, in the responder folder.\n" +
	"off: never (the default). The command, prompt, folder and limits are keys in config.json (see the docs)."

const autostartUsage = "usage: kwak config autostart [on|off]"

// configAutostart shows or sets whether a session's send may start the listener's user service
// when the listener is down (decision 46). On by default.
func (c *CLI) configAutostart(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 || len(args) == 1 && args[0] != "on" && args[0] != "off" {
		fmt.Fprintln(stderr, autostartUsage)
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
	if len(args) == 0 {
		state := "off"
		if cfg.Autostart() {
			state = "on"
		}
		fmt.Fprintf(stdout, "Listener autostart on this machine: %s.\n\n%s\n%s\n", state, autostartHelp, autostartUsage)
		return 0
	}
	on := args[0] == "on"
	cfg.ListenerAutostart = &on
	if err := cfg.Save(dir); err != nil {
		return fail(stderr, "could not write %s: %v", ConfigFile, err)
	}
	fmt.Fprintf(stdout, "Listener autostart on this machine is now %s.\n", args[0])
	return 0
}

const autostartHelp = "on (the default): when a session sends a question and the listener is down, kwak starts the\n" +
	"listener's user service, once per session. It never starts a listener that kwak install did not set up.\n" +
	"off: never; start the listener yourself."

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
