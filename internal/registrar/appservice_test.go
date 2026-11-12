package registrar

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
	wt "github.com/fcanela/kwak/internal/wire/wiretest"
)

func strp(s string) *string { return &s }

func TestTransactionAuth(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, header, query string
		want                int
	}{
		{"header", "Bearer " + testHS, "", 200},
		{"query", "", testHS, 200},
		{"both", "Bearer " + testHS, testHS, 200},
		{"header wrong", "Bearer nope", "", 403},
		{"query wrong", "", "nope", 403},
		{"both, query wrong", "Bearer " + testHS, "nope", 403},
		{"both, header wrong", "Bearer nope", testHS, 403},
		{"missing", "", "", 403},
	}
	for i, c := range cases {
		for _, path := range []string{"/_matrix/app/v1/transactions/a" + string(rune('a'+i)), "/_matrix/app/v1/ping"} {
			method := "PUT"
			if strings.HasSuffix(path, "ping") {
				method = "POST"
			}
			if c.query != "" {
				path += "?access_token=" + c.query
			}
			r := httptest.NewRequest(method, path, strings.NewReader(`{"events":[]}`))
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			w := httptest.NewRecorder()
			f.as.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("%s %s: %d, want %d", c.name, method, w.Code, c.want)
			}
			if c.want == 403 && !strings.Contains(w.Body.String(), `"M_FORBIDDEN"`) {
				t.Errorf("%s: body %s", c.name, w.Body)
			}
		}
	}
	for _, p := range []string{"/_matrix/app/v1/users/@x:kwak.test", "/_matrix/app/v1/rooms/%23x:kwak.test"} {
		r := httptest.NewRequest("GET", p, nil)
		r.Header.Set("Authorization", "Bearer "+testHS)
		w := httptest.NewRecorder()
		f.as.ServeHTTP(w, r)
		if w.Code != 404 || !strings.Contains(w.Body.String(), "M_NOT_FOUND") {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
}

func TestTransactionDedupeAndDoneEvents(t *testing.T) {
	f := newFixture(t)
	f.hs.addHuman(mx("bob"))
	ev := f.event(testAdmin, f.dm(testAdmin), "m.room.message", nil, map[string]string{"msgtype": "m.text", "body": "owner add " + mx("bob")})
	body, _ := json.Marshal(map[string]any{"events": []matrix.Event{ev}})
	send := func(txn string) {
		r := httptest.NewRequest("PUT", "/_matrix/app/v1/transactions/"+txn, strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+testHS)
		w := httptest.NewRecorder()
		f.as.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("txn %s: %d", txn, w.Code)
		}
	}
	send("same")
	send("same")
	if n := f.hs.callCount("CreateDM @bob"); n != 1 {
		t.Fatalf("a repeated txnId ran the command %d times", n)
	}
	// After a restart the txn cache is gone; done_events still stops a replay.
	f.reopen()
	send("same")
	send("other")
	if n := f.hs.callCount("CreateDM @bob"); n != 1 {
		t.Fatalf("a replayed event ran the command %d times", n)
	}
	if !f.state().done(ev.EventID) {
		t.Fatal("event id not in done_events")
	}
}

func TestForgedCommandsAreIgnored(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	f.hs.addHuman(mx("eve"))
	adminDM := f.dm(testAdmin)
	forge := func(sender, room, body string) []string {
		ev := f.event(sender, room, "m.room.message", nil, map[string]string{"msgtype": "m.text", "body": "help"})
		ev.Content, _ = json.Marshal(map[string]string{"msgtype": "m.text", "body": body})
		before := len(f.hs.noticesIn(room))
		f.txn(ev)
		return f.hs.noticesIn(room)[before:]
	}
	pr := f.startPair()
	for _, c := range []struct{ sender, room, body string }{
		{testAdmin, adminDM, "owner add " + mx("eve")},
		{mx("fcanela"), dm, "pair " + pr.Code},
		{mx("fcanela"), dm, "revoke all"},
		{mx("fcanela"), dm, "set question-timeout 2h"},
	} {
		if out := forge(c.sender, c.room, c.body); len(out) != 0 {
			t.Errorf("%q answered %q", c.body, out)
		}
	}
	st := f.state()
	if st.Owners["eve"] != nil || st.Pairings[p.id].Revoked != nil || st.Owners["fcanela"].Settings.QuestionTimeout != "15m" {
		t.Fatal("a forged command changed state")
	}
	if c := f.callJSON("POST", wire.PathPairClaim, pr.Claim, struct{}{}, nil); c != 202 {
		t.Fatalf("forged pair bound the code: %d", c)
	}
	// A wrong sender in tuwunel's copy also fails the check.
	ev := f.event(mx("eve"), dm, "m.room.message", nil, map[string]string{"msgtype": "m.text", "body": "revoke all"})
	ev.Sender = mx("fcanela")
	f.txn(ev)
	if f.state().Pairings[p.id].Revoked != nil {
		t.Fatal("forged sender revoked")
	}
}

func TestCommandsOnlyInTheRecordedDM(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	f.addOwner("bob")
	p := f.pair("fcanela", "", true)
	for _, room := range []string{testRoom, f.dm(mx("bob")), f.dm(testAdmin)} {
		f.hs.join(room, mx("fcanela"))
		if out := f.say(mx("fcanela"), room, "revoke all"); len(out) != 0 {
			t.Errorf("command in %s answered %q", room, out)
		}
	}
	if f.state().Pairings[p.id].Revoked != nil {
		t.Fatal("a command outside the recorded DM ran")
	}
	// Agents are never commanded through, and the bot never answers itself.
	f.hs.join(dm, mx(claudeLP))
	if out := f.say(mx(claudeLP), dm, "help"); len(out) != 0 {
		t.Errorf("agent got %q", out)
	}
	// While the DM does not have exactly two members, nothing runs.
	f.hs.mu.Lock()
	delete(f.hs.rooms[dm].members, mx(claudeLP))
	delete(f.hs.rooms[dm].members, mx("registrar"))
	f.hs.mu.Unlock()
	if out := f.say(mx("fcanela"), dm, "help"); len(out) != 0 {
		t.Errorf("command with one member answered %q", out)
	}
}

func TestUnknownAndRemovedSenders(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	f.removeOwner("fcanela")
	if out := f.say(mx("fcanela"), dm, "machines"); len(out) != 1 || out[0] != notOwner {
		t.Fatalf("removed owner got %q", out)
	}
	if out := f.say(testAdmin, f.dm(testAdmin), "frobnicate"); len(out) != 1 || !strings.HasPrefix(out[0], "Unknown command") {
		t.Fatalf("unknown command got %q", out)
	}
}

func TestOwnerAdd(t *testing.T) {
	f := newFixture(t)
	adminDM := f.dm(testAdmin)
	say := func(body string) string {
		out := f.say(testAdmin, adminDM, body)
		if len(out) != 1 {
			t.Fatalf("%q: replies %q", body, out)
		}
		return out[0]
	}
	f.hs.addHuman(mx("fcanela"))
	if got := say("owner add " + mx("fcanela") + " as other"); !strings.Contains(got, "can be an owner name as it is") {
		t.Errorf("alias for a fitting localpart: %q", got)
	}
	if got := say("owner add " + mx("fcanela")); got != "Added "+mx("fcanela")+" as owner fcanela." {
		t.Errorf("owner add: %q", got)
	}
	o := f.state().Owners["fcanela"]
	if o == nil || o.Human != mx("fcanela") || o.Alias || o.AddedBy != testAdmin || o.Settings != (OwnerSettings{QuestionTimeout: "15m"}) {
		t.Fatalf("owner %+v", o)
	}
	dm := f.dm(mx("fcanela"))
	if n := f.hs.noticesIn(dm); len(n) != 1 || !strings.HasPrefix(n[0], "You are now a kwak owner as `fcanela`.") {
		t.Errorf("owner DM notices %q", n)
	}
	for _, b := range f.hs.dmBody {
		pl := b["power_level_content_override"].(map[string]any)
		if _, ok := pl["users"]; ok {
			t.Fatal("DM body has a users key")
		}
	}
	if got := say("owner add " + mx("fcanela")); !strings.Contains(got, "already an owner") {
		t.Errorf("twice: %q", got)
	}

	f.hs.addHuman(mx("ana.lopez"))
	if got := say("owner add " + mx("ana.lopez")); !strings.Contains(got, "cannot be an owner name") {
		t.Errorf("dotted localpart without alias: %q", got)
	}
	f.hs.addHuman(mx("bob"))
	if got := say("owner add " + mx("ana.lopez") + " as bob"); !strings.Contains(got, "another person's user name") {
		t.Errorf("alias of another human: %q", got)
	}
	if got := say("owner add " + mx("ana.lopez") + " as fcanela"); !strings.Contains(got, "taken by") && !strings.Contains(got, "another person's") {
		t.Errorf("alias of another owner: %q", got)
	}
	if got := say("owner add " + mx("ana.lopez") + " as ana"); got != "Added "+mx("ana.lopez")+" as owner ana." {
		t.Errorf("alias: %q", got)
	}
	if o := f.state().Owners["ana"]; o == nil || !o.Alias {
		t.Fatalf("alias owner %+v", o)
	}
	if got := say("owner add " + mx("ghost")); !strings.Contains(got, "does not exist") {
		t.Errorf("unknown human: %q", got)
	}
	if got := say("owner add @x:elsewhere.example"); !strings.Contains(got, "not a user on") {
		t.Errorf("remote human: %q", got)
	}

	f.removeOwner("fcanela")
	if got := say("owner add " + mx("fcanela")); !strings.Contains(got, "--restore") {
		t.Errorf("removed owner without --restore: %q", got)
	}
	if got := say("owner add --restore " + mx("fcanela")); !strings.HasPrefix(got, "Restored owner fcanela") {
		t.Errorf("restore: %q", got)
	}
	if o := f.state().Owners["fcanela"]; o.Removed != nil {
		t.Fatal("still removed")
	}
}

func TestAdminWhoIsAlsoOwnerHasOneDM(t *testing.T) {
	f := newFixture(t)
	adminDM := f.dm(testAdmin)
	out := f.say(testAdmin, adminDM, "owner add "+testAdmin)
	if len(out) != 2 || out[0] != "You are now a kwak owner as `admin`. On your machine run `kwak setup` (or `kwak pair`) and send me the code it prints, here." {
		t.Fatalf("replies %q", out)
	}
	if f.dm(testAdmin) != adminDM || f.hs.callCount("CreateDM") != 1 {
		t.Fatal("a second DM was opened")
	}
	help := f.say(testAdmin, adminDM, "help")
	if len(help) != 1 || !strings.Contains(help[0], "owner add") || !strings.Contains(help[0], "pair <CODE>") {
		t.Fatalf("help %q", help)
	}
}

func TestInviteHandling(t *testing.T) {
	f := newFixture(t)
	bot := mx("registrar")
	invite := func(sender, room string) {
		f.txn(f.event(sender, room, "m.room.member", strp(bot), map[string]string{"membership": "invite"}))
	}
	f.hs.addHuman(mx("eve"))
	f.hs.rooms["!eve"] = &fakeRoom{members: map[string]bool{mx("eve"): true}}
	invite(mx("eve"), "!eve")
	if f.hs.member("!eve", bot) {
		t.Fatal("joined a stranger's room")
	}
	// An owner without a recorded DM (its creation failed) invites the bot to an encrypted room.
	f.hs.fail["CreateDM"] = mErr(500, "M_UNKNOWN")
	f.hs.addHuman(mx("fcanela"))
	f.say(testAdmin, f.dm(testAdmin), "owner add "+mx("fcanela"))
	delete(f.hs.fail, "CreateDM")
	if f.dm(mx("fcanela")) != "" {
		t.Fatal("DM recorded although its creation failed")
	}
	f.hs.rooms["!enc"] = &fakeRoom{members: map[string]bool{mx("fcanela"): true}, encrypted: true}
	invite(mx("fcanela"), "!enc")
	if n := f.hs.noticesIn("!enc"); len(n) != 1 || n[0] != "I cannot read encrypted or group chats. I opened a plain chat with you instead." {
		t.Fatalf("notice %q", n)
	}
	dm := f.dm(mx("fcanela"))
	if f.hs.member("!enc", bot) || dm == "" || dm == "!enc" {
		t.Fatalf("encrypted room: still member %v, DM %q", f.hs.member("!enc", bot), dm)
	}
	// With a DM recorded, further invites are ignored.
	f.hs.rooms["!plain"] = &fakeRoom{members: map[string]bool{mx("fcanela"): true}}
	invite(mx("fcanela"), "!plain")
	if f.hs.member("!plain", bot) || f.dm(mx("fcanela")) != dm {
		t.Fatal("an invite replaced the recorded DM")
	}
}

func TestInviteRecordsAPlainDM(t *testing.T) {
	f := newFixture(t)
	f.hs.fail["CreateDM"] = mErr(500, "M_UNKNOWN")
	f.hs.addHuman(mx("fcanela"))
	f.say(testAdmin, f.dm(testAdmin), "owner add "+mx("fcanela"))
	delete(f.hs.fail, "CreateDM")
	f.hs.rooms["!plain"] = &fakeRoom{members: map[string]bool{mx("fcanela"): true}}
	f.txn(f.event(mx("fcanela"), "!plain", "m.room.member", strp(mx("registrar")), map[string]string{"membership": "invite"}))
	if f.dm(mx("fcanela")) != "!plain" {
		t.Fatalf("DM %q", f.dm(mx("fcanela")))
	}
}

func TestDMLeaveInvitesBackThenOpensANewDM(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	leave := func(room string) {
		f.txn(f.event(mx("fcanela"), room, "m.room.member", strp(mx("fcanela")), map[string]string{"membership": "leave"}))
	}
	leave(dm)
	if f.hs.callCount("Invite @fcanela") != 1 || f.dm(mx("fcanela")) != dm {
		t.Fatal("not invited back once")
	}
	leave(dm)
	nd := f.dm(mx("fcanela"))
	if nd == dm || nd == "" || f.hs.callCount("Invite @fcanela") != 1 {
		t.Fatalf("second leave: DM %q", nd)
	}
	if f.hs.member(dm, mx("registrar")) {
		t.Error("the bot stayed in the old DM")
	}
	// The new DM gets the invite-back again.
	leave(nd)
	if f.hs.callCount("Invite @fcanela") != 2 {
		t.Fatal("the new DM has no invite-back")
	}
	// dm reset on request.
	out := f.say(testAdmin, f.dm(testAdmin), "dm reset fcanela")
	if len(out) != 1 || !strings.HasPrefix(out[0], "Opened a new chat") || f.dm(mx("fcanela")) == nd {
		t.Fatalf("dm reset %q", out)
	}
}

func TestAgentProfileReset(t *testing.T) {
	f, p := pairedFixture(t)
	a := f.mustAgent(p.key, wire.HarnessClaude, wt.ClaudeSession, "")
	f.hs.mu.Lock()
	f.hs.users[a.UserID].display = "alice"
	f.hs.users[a.UserID].avatar = "mxc://x/y"
	f.hs.mu.Unlock()
	f.clock.Advance(5 * 60e9)
	f.txn(f.event(a.UserID, testRoom, "m.room.member", strp(a.UserID),
		map[string]string{"membership": "join", "displayname": "alice", "avatar_url": "mxc://x/y"}))
	f.hs.mu.Lock()
	u := *f.hs.users[a.UserID]
	f.hs.mu.Unlock()
	if u.display != claudeLP || u.avatar != "" {
		t.Fatalf("profile %q %q", u.display, u.avatar)
	}
	if rec := f.state().Agents[claudeLP]; !rec.LastActive.Equal(f.clock.Now()) {
		t.Errorf("the agent's own event did not touch last_active: %v", rec.LastActive)
	}
	// The reset's own member event causes no further reset.
	n := f.hs.callCount("SetDisplayName")
	f.txn(f.event(a.UserID, testRoom, "m.room.member", strp(a.UserID), map[string]string{"membership": "join", "displayname": claudeLP}))
	if f.hs.callCount("SetDisplayName") != n {
		t.Fatal("reset loop")
	}
}

func TestSettingsCommands(t *testing.T) {
	f := newFixture(t)
	dm := f.addOwner("fcanela")
	p := f.pair("fcanela", "", true)
	say := func(b string) string { return f.say(mx("fcanela"), dm, b)[0] }
	if got := say("set question-timeout 90m"); !strings.Contains(got, "Question timeout is now 90m") {
		t.Errorf("%q", got)
	}
	for _, bad := range []string{"set question-timeout 30s", "set question-timeout 25h", "set colour blue"} {
		if got := say(bad); !strings.HasPrefix(got, "Use: ") {
			t.Errorf("%s: %q", bad, got)
		}
	}
	// Decision 44: background resume is set on each machine. The bot says so and stores nothing.
	for _, v := range []string{"replies", "all", "off"} {
		if got := say("set resume " + v); got != "Background resume is no longer set here. Set it on each machine with: kwak config resume" {
			t.Errorf("set resume %s: %q", v, got)
		}
	}
	if got := say("help"); strings.Contains(got, "resume") {
		t.Errorf("help still names resume: %q", got)
	}
	_, data := f.callRaw("GET", wire.PathMe, p.key, nil, nil)
	if !strings.Contains(string(data), `"settings":{"question_timeout":"90m"}`) {
		t.Errorf("GET /me %s", data)
	}
	if got := say("settings"); got != "Question timeout: 90m." {
		t.Errorf("%q", got)
	}
	if got := say("machines"); !strings.Contains(got, p.id+": laptop (linux)") {
		t.Errorf("machines %q", got)
	}
}
