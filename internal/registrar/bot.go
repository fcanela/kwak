package registrar

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

const notOwner = "You are not on the kwak owner list. Ask the server admin."

// botErr is a refusal the bot replies with as is.
type botErr string

func (e botErr) Error() string { return string(e) }

// command runs one bot command from a DM message (spec 3.5).
func (s *Server) command(ctx context.Context, ev matrix.Event, body string) {
	r := s.roleOf(ev.Sender)
	if r.dm == "" || r.dm != ev.RoomID {
		return
	}
	var done bool
	s.Store.View(func(st *State) { done = st.done(ev.EventID) })
	if done {
		return
	}
	members, enc, err := s.HS.RoomInfo(ctx, ev.RoomID)
	if err != nil {
		s.Log.Warn("room info", "room", ev.RoomID, "err", err)
		s.reply(ctx, ev.RoomID, "That failed: I could not check this chat. Try again.")
		return
	}
	if enc || members > 2 {
		s.notice(ctx, ev.RoomID, "I cannot read encrypted or group chats. I opened a plain chat with you instead.")
		if _, err := s.openDM(ctx, ev.Sender, ev.RoomID); err != nil {
			s.Log.Warn("open DM", "human", ev.Sender, "err", err)
		}
		return
	}
	if members != 2 {
		return
	}
	if !r.admin && r.owner == "" {
		s.reply(ctx, ev.RoomID, notOwner)
		return
	}
	f := strings.Fields(body)
	if len(f) == 0 {
		return
	}
	verb := strings.ToLower(f[0])
	args := f[1:]
	run, changes := s.lookupCommand(r, verb, args)
	if run == nil {
		s.reply(ctx, ev.RoomID, "Unknown command. Send help for the list.")
		return
	}
	if changes {
		got, err := s.fetchMatches(ctx, ev, body)
		if err != nil {
			s.reply(ctx, ev.RoomID, "That failed: I could not re-read your message. Try again.")
			return
		}
		if got == nil {
			return
		}
		ok, stale := true, false
		err = s.Store.Update(func(st *State) error {
			stale = got.TS < st.CommandTS[ev.RoomID]
			if st.done(ev.EventID) || stale {
				ok = false
				return nil
			}
			st.markDone(ev.EventID)
			st.CommandTS[ev.RoomID] = got.TS
			return nil
		})
		if stale {
			s.Log.Warn("command older than the last one run in this chat; ignored", "event", ev.EventID)
		}
		if err != nil || !ok {
			return
		}
	}
	out, err := run(ctx, ev.Sender)
	var be botErr
	switch {
	case errors.As(err, &be):
		out = string(be)
	case err != nil:
		s.Log.Warn("command failed", "event", ev.EventID, "err", err)
		out = fmt.Sprintf("That failed: %s. Try again.", reason(err))
	}
	s.reply(ctx, ev.RoomID, out)
}

func reason(err error) string {
	var me *matrix.Error
	if errors.As(err, &me) {
		return "the homeserver answered " + hsErrcode(err)
	}
	return strings.TrimPrefix(err.Error(), "registrar: ")
}

type cmdFunc func(ctx context.Context, sender string) (string, error)

// lookupCommand returns the command for verb, and whether it changes state.
func (s *Server) lookupCommand(r role, verb string, a []string) (cmdFunc, bool) {
	is := func(n int, words ...string) bool {
		if len(a) != n {
			return false
		}
		for i, w := range words {
			if w != "" && !strings.EqualFold(a[i], w) {
				return false
			}
		}
		return true
	}
	switch {
	case verb == "help":
		return func(context.Context, string) (string, error) { return helpText(r), nil }, false
	case r.admin && verb == "owners" && len(a) == 0:
		return s.cmdOwners, false
	case r.admin && verb == "owner" && len(a) == 2 && strings.EqualFold(a[0], "add"):
		return func(ctx context.Context, by string) (string, error) { return s.ownerAdd(ctx, by, a[1], "") }, true
	case r.admin && verb == "owner" && is(4, "add", "", "as"):
		return func(ctx context.Context, by string) (string, error) { return s.ownerAdd(ctx, by, a[1], a[3]) }, true
	case r.admin && verb == "owner" && is(3, "add", "--restore"):
		return func(ctx context.Context, by string) (string, error) { return s.ownerRestore(ctx, a[2]) }, true
	case r.admin && verb == "owner" && is(2, "remove"):
		return func(ctx context.Context, by string) (string, error) { return s.ownerRemove(ctx, a[1]) }, true
	case r.admin && verb == "dm" && is(2, "reset"):
		return func(ctx context.Context, by string) (string, error) { return s.dmReset(ctx, a[1]) }, true
	case r.admin && verb == "close" && len(a) == 1:
		return func(ctx context.Context, by string) (string, error) { return s.adminClose(ctx, a[0]) }, true
	case r.admin && verb == "revoke" && len(a) == 2 && strings.EqualFold(a[1], "all"):
		return func(ctx context.Context, by string) (string, error) { return s.revokeAll(ctx, a[0]) }, true
	case r.owner != "" && verb == "revoke" && is(1, "all"):
		return func(ctx context.Context, by string) (string, error) { return s.revokeAll(ctx, r.owner) }, true
	case (r.admin || r.owner != "") && verb == "revoke" && len(a) == 1:
		return func(ctx context.Context, by string) (string, error) { return s.revokeOne(ctx, r, a[0]) }, true
	case r.owner != "" && verb == "pair" && len(a) == 1:
		return func(ctx context.Context, by string) (string, error) { return s.bind(ctx, r.owner, a[0], false) }, true
	case r.owner != "" && verb == "pair" && is(2, "test"):
		return func(ctx context.Context, by string) (string, error) { return s.bind(ctx, r.owner, a[1], true) }, true
	case r.owner != "" && verb == "machines" && len(a) == 0:
		return func(context.Context, string) (string, error) { return s.cmdMachines(r.owner), nil }, false
	case r.owner != "" && verb == "agents" && len(a) == 0:
		return func(context.Context, string) (string, error) { return s.cmdAgents(r.owner), nil }, false
	case r.owner != "" && verb == "settings" && len(a) == 0:
		return func(context.Context, string) (string, error) { return s.cmdSettings(r.owner), nil }, false
	case r.owner != "" && verb == "set" && len(a) == 2:
		return func(context.Context, string) (string, error) { return s.cmdSet(r.owner, a[0], a[1]) }, true
	}
	return nil, false
}

func helpText(r role) string {
	var b strings.Builder
	b.WriteString("kwak registrar commands:")
	if r.admin {
		b.WriteString("\nowner add <@human> [as <name>]: make someone an owner (as <name> only when the user name cannot be an owner name)" +
			"\nowner add --restore <@human>: restore a removed owner" +
			"\nowner remove <name>: revoke every pairing and close every agent of an owner" +
			"\ndm reset <name>: open a new chat with an owner" +
			"\nowners: list owners" +
			"\nclose <mxid>: close one agent" +
			"\nrevoke <pairing-id> | revoke <owner> all: revoke pairings of any owner")
	}
	if r.owner != "" {
		b.WriteString("\npair <CODE>: pair the machine that printed the code" +
			"\npair test <CODE>: pair it as a test machine: its agents join only the test rooms, never the agents room" +
			"\nmachines: your paired machines (machine name and OS are what the machine said)" +
			"\nagents: your open agents" +
			"\nrevoke <pairing-id> | revoke all: revoke your pairings" +
			"\nset question-timeout <duration>: the no-reply notice time, 1m to 24h" +
			"\nsettings: your settings")
	}
	b.WriteString("\nhelp: this list")
	return b.String()
}

// ownerAdd is `owner add <@human> [as <name>]` (spec 3.5, DV-13).
func (s *Server) ownerAdd(ctx context.Context, by, human, alias string) (string, error) {
	lp, ok := localpart(human, s.Cfg.ServerName)
	if !ok || lp == "" {
		return "", botErr(fmt.Sprintf("%s is not a user on %s.", human, s.Cfg.ServerName))
	}
	name := lp
	fits := wire.OwnerName.MatchString(lp)
	switch {
	case alias != "" && fits:
		return "", botErr(fmt.Sprintf("%s can be an owner name as it is; as <name> is only for user names that cannot.", lp))
	case alias == "" && !fits:
		return "", botErr(fmt.Sprintf("The user name %s cannot be an owner name (lowercase letters, digits, _ and -, at most 32, no dots). Send: owner add %s as <name>", lp, human))
	case alias != "":
		if !wire.OwnerName.MatchString(alias) {
			return "", botErr(fmt.Sprintf("%q is not a valid owner name (lowercase letters, digits, _ and -, at most 32, no dots).", alias))
		}
		taken, err := s.HS.UserExists(ctx, s.Cfg.mxid(alias))
		if err != nil {
			return "", err
		}
		if taken {
			return "", botErr(fmt.Sprintf("%s is another person's user name; pick another alias.", alias))
		}
		name = alias
	}
	exists, err := s.HS.UserExists(ctx, human)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", botErr(fmt.Sprintf("%s does not exist on %s.", human, s.Cfg.ServerName))
	}
	now := s.now()
	err = s.Store.Update(func(st *State) error {
		if cur, o := st.ownerOf(human); o != nil {
			if o.Removed != nil {
				return botErr(fmt.Sprintf("%s was removed. To add them back send: owner add --restore %s", human, human))
			}
			return botErr(fmt.Sprintf("%s is already an owner as %s.", human, cur))
		}
		if o := st.Owners[name]; o != nil {
			return botErr(fmt.Sprintf("The owner name %s is taken by %s.", name, o.Human))
		}
		st.Owners[name] = &Owner{Human: human, Alias: alias != "", Added: now, AddedBy: by,
			Settings: OwnerSettings{QuestionTimeout: "15m"}}
		return nil
	})
	if err != nil {
		return "", err
	}
	dm, err := s.dmFor(ctx, human)
	if err != nil {
		return fmt.Sprintf("Added %s as owner %s, but I could not open a chat with them (%s). Send: dm reset %s", human, name, reason(err), name), nil
	}
	s.notice(ctx, dm, fmt.Sprintf("You are now a kwak owner as `%s`. On your machine run `kwak setup` (or `kwak pair`) and send me the code it prints, here.", name))
	return fmt.Sprintf("Added %s as owner %s.", human, name), nil
}

// dmFor returns the human's recorded DM, opening one if there is none.
func (s *Server) dmFor(ctx context.Context, human string) (string, error) {
	var dm string
	s.Store.View(func(st *State) { dm = st.DMs[human] })
	if dm != "" {
		return dm, nil
	}
	return s.openDM(ctx, human, "")
}

func (s *Server) ownerRestore(ctx context.Context, human string) (string, error) {
	var name string
	err := s.Store.Update(func(st *State) error {
		n, o := st.ownerOf(human)
		if o == nil || o.Removed == nil {
			return botErr(fmt.Sprintf("%s is not a removed owner.", human))
		}
		o.Removed, name = nil, n
		return nil
	})
	if err != nil {
		return "", err
	}
	if dm, err := s.dmFor(ctx, human); err == nil {
		s.notice(ctx, dm, fmt.Sprintf("You are a kwak owner again as `%s`. Your old pairings stay revoked: run kwak pair on each machine.", name))
	}
	return fmt.Sprintf("Restored owner %s (%s). The old pairings stay revoked.", name, human), nil
}

// ownerRemove revokes every pairing, closes every agent and every listener account (spec 3.5).
func (s *Server) ownerRemove(ctx context.Context, name string) (string, error) {
	now := s.now()
	var pairings []string
	err := s.Store.Update(func(st *State) error {
		o := st.Owners[name]
		if o == nil || o.Removed != nil {
			return botErr(fmt.Sprintf("No owner %s.", name))
		}
		o.Removed = &now
		for id, p := range st.Pairings {
			if p.Owner == name && p.Revoked == nil {
				pairings = append(pairings, id)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(pairings)
	for _, id := range pairings {
		if err := s.revoke(ctx, id, "owner_removed"); err != nil {
			return "", err
		}
	}
	var lps []string
	s.Store.View(func(st *State) {
		for lp, a := range st.Agents {
			if a.Owner == name && a.Closed == nil {
				lps = append(lps, lp)
			}
		}
	})
	sort.Strings(lps)
	if _, err := s.closeAgents(ctx, lps, "owner_removed", "", nil); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed owner %s: %d pairings revoked, every agent closed. The names stay reserved.", name, len(pairings)), nil
}

func (s *Server) dmReset(ctx context.Context, name string) (string, error) {
	var human, old string
	s.Store.View(func(st *State) {
		if o := st.Owners[name]; o != nil {
			human, old = o.Human, st.DMs[o.Human]
		}
	})
	if human == "" {
		return "", botErr(fmt.Sprintf("No owner %s.", name))
	}
	if _, err := s.openDM(ctx, human, old); err != nil {
		return "", err
	}
	return fmt.Sprintf("Opened a new chat with %s.", human), nil
}

func (s *Server) adminClose(ctx context.Context, uid string) (string, error) {
	lp := s.agentLocalpart(uid)
	var ok bool
	s.Store.View(func(st *State) {
		a := st.Agents[lp]
		ok = a != nil && a.Kind != wire.KindStanding && a.Session != "" && a.Closed == nil
	})
	if !ok {
		return "", botErr(fmt.Sprintf("No open agent %s.", uid))
	}
	closed, err := s.closeAgents(ctx, []string{lp}, "admin", "", nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Closed %d agent(s): %s.", len(closed), strings.Join(closed, ", ")), nil
}

func (s *Server) revokeOne(ctx context.Context, r role, id string) (string, error) {
	var owner string
	var revoked bool
	s.Store.View(func(st *State) {
		if p := st.Pairings[id]; p != nil {
			owner, revoked = p.Owner, p.Revoked != nil
		}
	})
	if owner == "" || (!r.admin && owner != r.owner) {
		return "", botErr(fmt.Sprintf("No pairing %s of yours.", id))
	}
	if revoked {
		return fmt.Sprintf("Pairing %s was already revoked.", id), nil
	}
	if err := s.revoke(ctx, id, "revoked"); err != nil {
		return "", err
	}
	return fmt.Sprintf("Revoked pairing %s. Its key, its listener account and its agents are closed.", id), nil
}

func (s *Server) revokeAll(ctx context.Context, owner string) (string, error) {
	var ids []string
	var known bool
	s.Store.View(func(st *State) {
		_, known = st.Owners[owner]
		for id, p := range st.Pairings {
			if p.Owner == owner && p.Revoked == nil {
				ids = append(ids, id)
			}
		}
	})
	if !known {
		return "", botErr(fmt.Sprintf("No owner %s.", owner))
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.revoke(ctx, id, "revoked"); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Revoked %d pairing(s) of %s.", len(ids), owner), nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func (s *Server) cmdOwners(context.Context, string) (string, error) {
	var lines []string
	s.Store.View(func(st *State) {
		for name, o := range st.Owners {
			pairs, open := 0, 0
			var last time.Time
			for _, p := range st.Pairings {
				if p.Owner == name && p.Revoked == nil {
					pairs++
				}
			}
			for _, a := range st.Agents {
				if a.Owner != name {
					continue
				}
				if a.Kind != wire.KindStanding && a.Open() {
					open++
				}
				if a.LastActive.After(last) {
					last = a.LastActive
				}
			}
			state := ""
			if o.Removed != nil {
				state = " (removed)"
			}
			lines = append(lines, fmt.Sprintf("%s%s: %s, %d pairings, %d open agents, last activity %s",
				name, state, o.Human, pairs, open, stamp(last)))
		}
	})
	if len(lines) == 0 {
		return "No owners yet.", nil
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n"), nil
}

func (s *Server) cmdMachines(owner string) string {
	var lines []string
	s.Store.View(func(st *State) {
		for id, p := range st.Pairings {
			if p.Owner != owner {
				continue
			}
			state := "expires " + day(p.Expires)
			if p.Revoked != nil {
				state = "revoked " + day(*p.Revoked) + " (" + p.RevokeReason + ")"
			}
			kind := ""
			if p.Test {
				kind = " [test]"
			}
			// The pairing's listener account (decision 42), named by its word pair.
			as := ""
			if l := pairLabel(p.Listener); l != "" {
				as = " as " + l
			}
			lines = append(lines, fmt.Sprintf("%s%s: %s (%s)%s, created %s, %s, last used %s from %s",
				id, kind, p.Machine, p.OS, as, day(p.Created), state, stamp(p.LastUsed), p.LastIP))
		}
	})
	if len(lines) == 0 {
		return "No paired machines. Run kwak pair on a machine."
	}
	sort.Strings(lines)
	return "Machine names and OS are what each machine said about itself.\n" + strings.Join(lines, "\n")
}

func (s *Server) cmdAgents(owner string) string {
	var lines []string
	s.Store.View(func(st *State) {
		for lp, a := range st.Agents {
			if a.Owner == owner && a.Kind != wire.KindStanding && a.Open() {
				m := "?"
				if p := st.Pairings[a.Pairing]; p != nil {
					m = p.Machine
					if p.Test {
						m += ", test"
					}
				}
				lines = append(lines, fmt.Sprintf("%s (%s): %s, last active %s", a.Pairing, m, lp, stamp(a.LastActive)))
			}
		}
	})
	if len(lines) == 0 {
		return "No open agents."
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (s *Server) cmdSettings(owner string) string {
	var set OwnerSettings
	s.Store.View(func(st *State) {
		if o := st.Owners[owner]; o != nil {
			set = o.Settings
		}
	})
	return fmt.Sprintf("Question timeout: %s.", set.QuestionTimeout)
}

func (s *Server) cmdSet(owner, what, value string) (string, error) {
	var reply string
	var apply func(*OwnerSettings)
	switch strings.ToLower(what) {
	case "resume":
		// Decision 44: background resume is set on each machine, not here. An owner who still
		// sends this gets told where it went; nothing is stored.
		return "", botErr("Background resume is no longer set here. Set it on each machine with: kwak config resume")
	case "question-timeout":
		d, err := time.ParseDuration(value)
		if err != nil || d < time.Minute || d > 24*time.Hour {
			return "", botErr("Use: set question-timeout <duration>, between 1m and 24h, for example 15m")
		}
		v := fmtDuration(d)
		apply = func(st *OwnerSettings) { st.QuestionTimeout = v }
		reply = fmt.Sprintf("Question timeout is now %s. It takes effect on your machines within an hour.", v)
	default:
		return "", botErr("Use: set question-timeout <duration>")
	}
	err := s.Store.Update(func(st *State) error {
		o := st.Owners[owner]
		if o == nil {
			return errors.New("no such owner")
		}
		apply(&o.Settings)
		return nil
	})
	return reply, err
}

func fmtDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}
