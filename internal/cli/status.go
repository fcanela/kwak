package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

const shortTime = "2006-01-02 15:04 UTC"

func stamp(t time.Time) string { return t.UTC().Format(shortTime) }

// Status is kwak status: the pairing, the listener and its sessions.
func (c *CLI) Status(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: kwak status")
		return 2
	}
	c.printDev(stdout)
	var st wire.StatusResponse
	if err := c.call(context.Background(), wire.OpStatus, &st); err != nil {
		c.printPairing(stdout)
		return fail(stderr, "%s", listenerDown)
	}
	if !st.OK {
		return fail(stderr, "the listener refused the status request (%s)", st.Err)
	}
	printStatus(stdout, st)
	return 0
}

// printDev says, in a dev build, which folders it uses instead of the real ones (decision 40).
func (c *CLI) printDev(w io.Writer) {
	if !c.env.Dev {
		return
	}
	cdir, _ := c.env.ConfigDir()
	sdir, _ := c.env.StateDir()
	fmt.Fprintf(w, "Dev build: it uses its own test folder (config %s, state %s), not a real pairing's.\n", cdir, sdir)
}

func (c *CLI) printPairing(w io.Writer) {
	dir, err := c.env.ConfigDir()
	if err != nil {
		return
	}
	cfg, err := loadConfig(dir, c.env.InsecureHTTPForTests)
	switch {
	case errors.Is(err, ErrNotPaired):
		fmt.Fprintln(w, "This machine is not paired. Run kwak setup.")
	case err != nil:
		fmt.Fprintf(w, "The config cannot be read: %v\n", err)
	default:
		kind := ""
		if cfg.Test {
			kind = ", a test pairing"
		}
		fmt.Fprintf(w, "This machine is paired as %s on %s (pairing %s, server %s%s).\n", cfg.Owner, cfg.Machine, cfg.PairingID, cfg.Server, kind)
	}
}

var stateText = map[string]string{
	wire.StateUnpaired:       "not paired",
	wire.StateWaitingKeyring: "waiting for the keyring",
	wire.StateConnecting:     "connecting",
	wire.StateSyncing:        "syncing",
}

func printStatus(w io.Writer, st wire.StatusResponse) {
	state := stateText[st.State]
	if state == "" {
		state = st.State
	}
	line := "Listener: " + state
	if !st.LastSync.IsZero() {
		line += ", last sync " + stamp(st.LastSync)
	}
	fmt.Fprintf(w, "%s (kwak %s)\n", line, st.Kwak)
	if st.Detail != "" {
		fmt.Fprintf(w, "  %s\n", st.Detail)
	}
	if st.SyncError != "" {
		fmt.Fprintf(w, "  Last error: %s\n", st.SyncError)
	}
	if st.StateReset {
		fmt.Fprintln(w, "  State reset: messages since the last good state may be missed.")
	}
	if st.Owner != "" {
		kind := ""
		if st.Test {
			kind = " (a test pairing: only the test rooms)"
		}
		fmt.Fprintf(w, "Pairing: %s on %s, pairing %s, server %s%s\n", st.Owner, st.Machine, st.PairingID, st.Server, kind)
	}
	if st.ListenerAccount != "" {
		fmt.Fprintf(w, "Listener account: %s\n", st.ListenerAccount)
	}
	if !st.KeyExpiresAt.IsZero() {
		fmt.Fprintf(w, "Key valid until: %s\n", st.KeyExpiresAt.UTC().Format("2006-01-02"))
	}
	if st.Settings != (wire.Settings{}) {
		fmt.Fprintf(w, "Owner settings: question timeout %s\n", st.Settings.QuestionTimeout)
	}
	if st.Resume != "" {
		fmt.Fprintf(w, "Background resume on this machine: %s (Claude Code sessions only); %d runs, %d running\n",
			st.Resume, st.ResumeRuns, st.ResumeActive)
	}
	if st.Responder != "" {
		line := "Responder on this machine: " + st.Responder
		if st.Responder == "on" {
			line += ", folder " + st.ResponderDir
			if st.ResponderOwn {
				line += ", answers your own agents too"
			}
		}
		fmt.Fprintln(w, line)
		if st.ResponderAccount != "" {
			fmt.Fprintf(w, "Responder account: %s\n", st.ResponderAccount)
		}
		if st.ResponderError != "" {
			fmt.Fprintf(w, "  The responder account is not ready: %s\n", st.ResponderError)
		}
	}
	if st.ResumeHourLimit > 0 {
		fmt.Fprintf(w, "Hourly limit: %d background runs started in the last hour; the next may start at %s\n",
			st.ResumeHourLimit, stamp(st.ResumeNextAt))
	}
	for _, p := range st.Paused {
		fmt.Fprintf(w, "Loop ceiling: no more background runs for thread %s after %d; a message from a person in it starts the count again\n",
			p.Thread, p.Turns)
	}
	printChecks(w, st)
	fmt.Fprintf(w, "Caller checks: %s\n", st.CallerChecks)
	if len(st.Sessions) == 0 {
		fmt.Fprintln(w, "Sessions: none")
		return
	}
	fmt.Fprintf(w, "Sessions: %d\n", len(st.Sessions))
	for _, s := range st.Sessions {
		fmt.Fprintf(w, "  %s in %s\n", s.Key, s.Cwd)
		var parts []string
		if s.Agent != "" {
			parts = append(parts, "agent "+s.Agent)
		} else {
			parts = append(parts, "no agent yet")
		}
		if len(s.Helpers) > 0 {
			parts = append(parts, "helpers "+strings.Join(s.Helpers, ", "))
		}
		if s.Tools {
			parts = append(parts, "tools connected")
		} else {
			parts = append(parts, "no tools connection")
		}
		if s.Volunteer {
			parts = append(parts, "volunteers")
		}
		parts = append(parts, fmt.Sprintf("inbox %d", s.Inbox))
		fmt.Fprintf(w, "    %s\n", strings.Join(parts, "; "))
		times := "started " + stamp(s.Started)
		if !s.LastHook.IsZero() {
			times += ", last hook " + stamp(s.LastHook)
		}
		if !s.Ended.IsZero() {
			times += ", ended " + stamp(s.Ended)
		}
		fmt.Fprintf(w, "    %s\n", times)
		if s.Note != "" {
			fmt.Fprintf(w, "    %s\n", s.Note)
		}
	}
}

// printChecks shows the message checks (spec 5.16) and the latest messages they stopped.
func printChecks(w io.Writer, st wire.StatusResponse) {
	cmd := func(p string) string {
		if p == "" {
			return "off"
		}
		return p
	}
	line := fmt.Sprintf("Checks: secrets scan on; outgoing command %s; incoming command %s", cmd(st.CheckOutgoing), cmd(st.CheckIncoming))
	if st.CheckWaiting > 0 {
		line += fmt.Sprintf("; %d waiting for the incoming check", st.CheckWaiting)
	}
	fmt.Fprintln(w, line)
	for _, r := range st.Rejected {
		switch r.Way {
		case "outgoing":
			where := ""
			if r.Thread != "" {
				where = " in thread " + r.Thread
			}
			fmt.Fprintf(w, "  %s refused a %s by %s%s (not sent): %s\n", stamp(r.At), r.Tool, r.Agent, where, r.Reason)
		default:
			fmt.Fprintf(w, "  %s held a message from %s in thread %s for %s: %s\n", stamp(r.At), r.Sender, r.Thread, r.Agent, r.Reason)
		}
	}
}

// Inbox is kwak inbox: waiting items per session, with the command that resumes each one.
func (c *CLI) Inbox(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: kwak inbox")
		return 2
	}
	var r wire.InboxResponse
	if err := c.call(context.Background(), wire.OpInbox, &r); err != nil {
		return fail(stderr, "%s", listenerDown)
	}
	if !r.OK {
		return fail(stderr, "the listener refused the inbox request (%s)", r.Err)
	}
	if len(r.Sessions) == 0 {
		fmt.Fprintln(stdout, "No messages are waiting.")
		return 0
	}
	for _, s := range r.Sessions {
		n := "1 message"
		if s.Items != 1 {
			n = fmt.Sprintf("%d messages", s.Items)
		}
		state := "live"
		if s.Ended {
			state = "ended"
		}
		who := s.Agent
		if who == "" {
			who = "no agent yet"
		}
		fmt.Fprintf(stdout, "%s for %s (%s, %s), in %s\n", n, s.Key, who, state, s.Cwd)
		if s.Resume != "" {
			fmt.Fprintf(stdout, "  resume: %s\n", s.Resume)
		}
	}
	return 0
}
