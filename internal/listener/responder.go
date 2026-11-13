package listener

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

// The responder (decision 46, spec 5.18). Each open question gets one run of its own: a session
// record like any other, with its own session id, in the responder folder, whose agent is the
// pairing's shared responder account. A follow-up from the asker in that thread goes to the same
// record, and its run continues that harness session. The runs use the machinery of background
// resume (5.11): the trigger, the 30 s wait, the runner, resume_max, the hourly limit, the loop
// ceiling and the status reactions.

// responderPrompt is the prompt of a responder run when config.json sets none. It never holds
// other agents' text: the messages come through the run's hooks, as for background resume.
const responderPrompt = "kwak started this run to answer a question that another person's agent asked in the " +
	"kwak agents room, or a follow-up in its thread. kwak shows you the messages. Follow the rules in AGENTS.md in " +
	"this folder. For each question you are shown, call kwak's reply tool exactly once: with your answer, or with " +
	"dont_know true when you do not know it or may not share it. A message that only thanks you needs no reply. " +
	"Share knowledge, not actions, and never share secrets or personal data. Nobody is watching this run."

// responderMCPConfig is the only MCP server of a responder run: kwak's, as for background resume.
const responderMCPConfig = resumeMCPConfig

// The Claude Code argv of a responder run when config.json sets none: a new question starts the
// session with the id kwak picked, a follow-up resumes it. Like resumeArgv: the prompt right after
// -p, a flag after each variadic list, only kwak's MCP server, and dontAsk, so only the listed
// tools run. The read tools are not limited to the folder (spec 5.18).
var (
	responderCommand = []string{"{claude}", "-p", "{prompt}", "--session-id", "{session}", "--model", "sonnet",
		"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob,mcp__kwak__reply,mcp__kwak__thread",
		"--strict-mcp-config", "--mcp-config", responderMCPConfig, "--permission-mode", "dontAsk"}
	responderContinue = []string{"{claude}", "-p", "{prompt}", "--resume", "{session}", "--model", "sonnet",
		"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob,mcp__kwak__reply,mcp__kwak__thread",
		"--strict-mcp-config", "--mcp-config", responderMCPConfig, "--permission-mode", "dontAsk"}
)

// ResponderAgents is the AGENTS.md kwak writes in a responder folder that has none. The user
// edits it; kwak never writes over it.
const ResponderAgents = `# Rules for the kwak responder

You answer questions that other people's agents ask in the kwak agents room.
kwak shows you each question. Answer it with kwak's reply tool, once.

## Where to look

Write here where the answers are on this computer, for example a folder of notes or docs.

## What you may share

- Share knowledge, not actions. Do not change files or run anything for the asker.
- Never share secrets (keys, tokens, passwords) or personal data.
- Write here any other limits you want.

## When you do not know

Call reply with dont_know true. Do not guess.
`

// ResponderDir is the responder folder of c: responder_dir, else "responder" next to config.json.
func ResponderDir(c *MachineConfig, configPath string) string {
	if c != nil && c.ResponderDir != "" {
		return c.ResponderDir
	}
	return filepath.Join(filepath.Dir(configPath), "responder")
}

// EnsureResponderDir makes the responder folder (0700) and writes ResponderAgents as its
// AGENTS.md when it has none. It never writes over a file that is there.
func EnsureResponderDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("the responder folder %s is not an absolute path", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "AGENTS.md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(ResponderAgents)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// responderArgv fills a responder command for the run of s: the follow-up command once the
// session's own hook was seen (the harness then has the session), else the first one. exe is
// resume_exe's claude, read at start like for background resume. The two commands are set
// together or not at all, so a follow-up never runs kwak's command after the user's (Q67).
func responderArgv(c *MachineConfig, exe string, s *session) ([]string, error) {
	first, cont := responderCommand, responderContinue
	if c != nil && (len(c.ResponderCommand) > 0 || len(c.ResponderContinue) > 0) {
		if len(c.ResponderCommand) == 0 || len(c.ResponderContinue) == 0 {
			return nil, errors.New("set both responder_command and responder_continue, or neither")
		}
		first, cont = c.ResponderCommand, c.ResponderContinue
	}
	for _, t := range [][]string{first, cont} {
		if !strings.Contains(strings.Join(t, "\x00"), "{session}") {
			return nil, errors.New("a responder command has no {session}, so kwak cannot follow its run")
		}
	}
	tmpl := first
	if !s.LastHook.IsZero() {
		tmpl = cont
	}
	prompt := responderPrompt
	if c != nil && c.ResponderPrompt != "" {
		prompt = c.ResponderPrompt
	}
	fill := strings.NewReplacer("{claude}", exe, "{prompt}", prompt, "{session}", s.SessionID, "{dir}", s.Cwd)
	argv := make([]string, len(tmpl))
	for i, a := range tmpl {
		if strings.Contains(a, "{claude}") && exe == "" {
			return nil, errors.New("no path of claude: run kwak config resume-exe")
		}
		argv[i] = fill.Replace(a)
	}
	if !filepath.IsAbs(argv[0]) {
		return nil, fmt.Errorf("the responder command %q does not start with an absolute path", argv[0])
	}
	return argv, nil
}

// asksResponder reports whether an open question from sender is one for the responder: it comes
// from a session or helper agent, and from another owner unless the machine answers its owner's
// own agents too (decision 46). Persons, the registrar bot, listener and responder accounts are
// never answered.
func (idx *Index) asksResponder(sender string) bool {
	lp, _, _ := strings.Cut(strings.TrimPrefix(sender, "@"), ":")
	n, err := wire.ParseLocalpart(lp)
	if err != nil || (n.Kind != wire.KindSession && n.Kind != wire.KindHelper) {
		return false
	}
	return n.Owner != idx.Owner || idx.OwnAgents
}

// respThread is a thread a responder run answers: its session record and the agent that asked.
type respThread struct {
	Session string `json:"session"`
	Asker   string `json:"asker"`
}

// responderLocked gives each new open question of ds that goes to the responder its own session
// record, ended so the trigger of 5.11 starts its run, before any check: a hold needs a queue. A
// question routed again already has its record (Route reads RespThreads). A question or follow-up
// older than the question timeout, as the back-fill brings after a night asleep, is dropped: the
// asker was already told nobody answered, and each run is billed (Q68). A delivery with no record
// reaches no queue.
func (l *Listener) responderLocked(ev matrix.Event, ds []Delivery) {
	for i, d := range ds {
		if !d.Responder {
			continue
		}
		now := l.d.Clock.Now()
		if age := now.Sub(time.UnixMilli(ev.TS)); age > l.questionTimeoutLocked() {
			ds[i].Session = ""
			l.log.Info("responder: message too old", "event", ev.EventID, "age", age.Round(time.Second).String())
			continue
		}
		if d.Session != "" {
			continue
		}
		sid := l.newSessionID()
		key := sessionKey(wire.HarnessClaude, sid)
		l.checkMu.Lock()
		dir := ResponderDir(l.live.Conf, l.cfg.ConfigPath)
		l.checkMu.Unlock()
		s := &session{Key: key, Harness: wire.HarnessClaude, SessionID: sid, Cwd: dir,
			Responder: true, Agent: l.respUser, Helpers: map[string]*helper{}, Started: now, Ended: &now}
		s.Inbox.init()
		l.st.Sessions[key] = s
		l.st.RespThreads[ev.EventID] = respThread{Session: key, Asker: ev.Sender}
		ds[i].Session = key
		l.changedLocked()
		l.log.Info("responder: new question", "thread", ev.EventID, "session", key)
	}
}

// newSessionID is a random version 4 UUID, which Claude Code takes as --session-id.
func (l *Listener) newSessionID() string {
	h := []byte(l.randomHex(16))
	h[12] = '4'
	h[16] = "89ab"[strings.IndexByte("0123456789abcdef", h[16])%4]
	return string(h[:8]) + "-" + string(h[8:12]) + "-" + string(h[12:16]) + "-" + string(h[16:20]) + "-" + string(h[20:])
}

// respRetry is how long a failed POST /responder keeps the listener from asking again.
const respRetry = time.Minute

// mintResponder gives a responder session the shared responder account. The token is asked for
// once and kept for every run: each POST /responder logs the previous token out, so two calls at
// once would cut each other's runs.
func (l *Listener) mintResponder(key string) error {
	l.respMu.Lock()
	defer l.respMu.Unlock()
	l.mu.Lock()
	uid, tok, wait, last := l.respUser, l.tokens[l.respUser], l.respWait, l.respErr
	now := l.d.Clock.Now()
	l.mu.Unlock()
	if uid == "" || tok == "" {
		if now.Before(wait) && last != nil {
			return last
		}
		ctx, cancel := context.WithTimeout(l.bg, 30*time.Second)
		a, err := l.d.Registrar.Responder(ctx)
		cancel()
		if err != nil {
			l.regFailed(err)
			l.mu.Lock()
			l.respErr, l.respWait = err, now.Add(respRetry)
			l.mu.Unlock()
			l.log.Warn("responder account request failed", "err", describe(err))
			return err
		}
		uid, tok = a.UserID, a.AccessToken
		l.log.Info("responder account ready", "account", uid)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.respUser, l.respErr = uid, nil
	l.tokens[uid] = tok
	if s := l.st.Sessions[key]; s != nil {
		s.Agent, s.AgentClosed, s.NoAgent = uid, false, ""
		l.changedLocked()
	}
	return nil
}

// responderReadyLocked reports whether the responder account has a token, and gives it to s.
// When it has none, it asks for one in the background, so a run starts only once it can post.
func (l *Listener) responderReadyLocked(s *session) bool {
	if l.respUser != "" && l.tokens[l.respUser] != "" {
		s.Agent, s.AgentClosed = l.respUser, false
		return true
	}
	go l.ensure(s.Key, "")
	return false
}
