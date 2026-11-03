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
		fmt.Fprintf(w, "This machine is paired as %s on %s (pairing %s, server %s).\n", cfg.Owner, cfg.Machine, cfg.PairingID, cfg.Server)
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
		fmt.Fprintf(w, "Pairing: %s on %s, pairing %s, server %s\n", st.Owner, st.Machine, st.PairingID, st.Server)
	}
	if !st.KeyExpiresAt.IsZero() {
		fmt.Fprintf(w, "Key valid until: %s\n", st.KeyExpiresAt.UTC().Format("2006-01-02"))
	}
	if st.Settings != (wire.Settings{}) {
		fmt.Fprintf(w, "Owner settings: background resume %s, question timeout %s\n", st.Settings.Resume, st.Settings.QuestionTimeout)
	}
	if st.Resume != "" {
		fmt.Fprintf(w, "Resume: %s; %d runs, %d running\n", st.Resume, st.ResumeRuns, st.ResumeActive)
	}
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
