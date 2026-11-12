//go:build integration && linux

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/test/integration/ittest"
)

// F1: tuwunel loads the JSON registration from kwak.yaml, and appservice /register works.
func TestF1RegistrationLoads(t *testing.T) {
	p := startProbe(t, "F1")
	if p == nil {
		return
	}
	raw, err := os.ReadFile(ittest.RegistrationPath)
	p.must(err, "read the registration")
	p.note("registration file", "%s is JSON: %v", ittest.RegistrationPath, json.Valid(raw) && raw[0] == '{')

	st, uid, code := p.whoami(p.env.Registration.ASToken, "")
	p.check(st == http.StatusOK && uid == mxid("registrar"), "as_token whoami",
		"status %d, user %q, errcode %q", st, uid, code)

	l, err := p.as().RegisterAppservice(p.ctx, "agent.f1.listener", "KWAK-f1")
	p.check(err == nil && l.UserID == mxid("agent.f1.listener") && l.AccessToken != "" && l.DeviceID == "KWAK-f1",
		"appservice /register", "%s, user %q, device %q, token set %v", describe(err), l.UserID, l.DeviceID, l.AccessToken != "")
	if err == nil {
		st, uid, code = p.whoami(l.AccessToken, "")
		p.check(st == http.StatusOK && uid == l.UserID, "registered token works",
			"whoami status %d, user %q, errcode %q", st, uid, code)
	}
	yes, err1 := p.as().ProfileExists(p.ctx, mxid("agent.f1.listener"))
	no, err2 := p.as().ProfileExists(p.ctx, mxid("agent.f1.nobody"))
	p.note("ProfileExists with the as_token", "registered user %v (%s), unknown user %v (%s)", yes, describe(err1), no, describe(err2))
}

// F2: the appservice ping makes tuwunel call the runner's /_matrix/app/v1/ping by docker name.
func TestF2AppservicePing(t *testing.T) {
	p := startProbe(t, "F2")
	if p == nil {
		return
	}
	rc := p.serveRecorder()
	p.note("network layout", "%s; tuwunel calls %s", netMode(), p.env.Registration.URL)

	st, b := p.raw(http.MethodPost, "/_matrix/client/v1/appservice/kwak/ping", p.env.Registration.ASToken, true,
		map[string]any{"transaction_id": "kwak-f2"})
	var r struct {
		DurationMS *int64 `json:"duration_ms"`
		Errcode    string `json:"errcode"`
	}
	json.Unmarshal(b, &r)
	p.check(st == http.StatusOK && r.DurationMS != nil, "ping answer", "status %d, duration_ms set %v, errcode %q",
		st, r.DurationMS != nil, r.Errcode)

	h, ok := rc.waitHit(p.ctx, "/_matrix/app/v1/ping", 10*time.Second)
	p.check(ok && h.Method == http.MethodPost, "tuwunel called the runner by name", "seen %v, method %q", ok, h.Method)
	if ok {
		p.note("ping auth", "Authorization Bearer hs_token: present %v, matches %v; ?access_token: present %v, matches %v",
			h.BearerPresent, h.BearerMatches, h.QueryPresent, h.QueryMatches)
	}
}

// F3: /register and /login without X-Forwarded-For fail; with it they work.
func TestF3ForwardedFor(t *testing.T) {
	p := startProbe(t, "F3")
	if p == nil {
		return
	}
	noXFF := p.as()
	noXFF.ForwardedFor = ""

	_, err := noXFF.RegisterAppservice(p.ctx, "agent.f3.a", "")
	p.check(err != nil, "appservice /register without XFF fails", "%s", describe(err))

	l, err := p.as().RegisterAppservice(p.ctx, "agent.f3.b", "")
	p.check(err == nil, "appservice /register with XFF works", "%s", describe(err))

	_, err = noXFF.LoginAppservice(p.ctx, mxid("agent.f3.b"), "")
	p.check(err != nil, "appservice /login without XFF fails", "%s", describe(err))

	_, err = p.as().LoginAppservice(p.ctx, mxid("agent.f3.b"), "")
	p.check(err == nil, "appservice /login with XFF works", "%s", describe(err))

	st, b := p.raw(http.MethodPost, v3+"/register?kind=user", "", false,
		map[string]any{"username": "f3human", "password": "f3-password-not-secret"})
	p.check(st != http.StatusUnauthorized && st != http.StatusOK, "human /register without XFF fails",
		"status %d: %s", st, truncate(b))
	st, b = p.raw(http.MethodPost, v3+"/register?kind=user", "", true,
		map[string]any{"username": "f3human", "password": "f3-password-not-secret"})
	p.check(st == http.StatusUnauthorized, "human /register with XFF reaches the token stage",
		"status %d: %s", st, truncate(b))

	if l.AccessToken != "" {
		req, _ := http.NewRequestWithContext(p.ctx, http.MethodGet, p.env.Homeserver+v3+"/account/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+l.AccessToken)
		resp, err := p.env.HTTP.Do(req)
		p.must(err, "whoami without XFF")
		resp.Body.Close()
		p.note("other routes without XFF", "whoami status %d", resp.StatusCode)
	}
}

// F4: a passwordless agent joins the public_chat room through ?user_id=; its own-token join is
// refused; it sends with its own token.
func TestF4AgentJoin(t *testing.T) {
	p := startProbe(t, "F4")
	if p == nil {
		return
	}
	room := p.agentsRoom()
	st, _ := p.state(p.env.Admin.AccessToken, room, "m.room.guest_access", "")
	p.note("guest_access state in the 3.11 room", "status %d", st)
	_, err := p.as().Join(p.ctx, room)
	p.must(err, "registrar joins")

	const lp = "agent.alice.0123456789abcdef"
	a := p.agent(lp, "")
	own := p.client(a.AccessToken)

	_, err = own.Join(p.ctx, room)
	p.check(err != nil, "own-token join refused", "%s", describe(err))

	_, err = p.asUser(a.UserID).Join(p.ctx, room)
	p.check(err == nil, "as_token ?user_id= join", "%s", describe(err))

	members, err := p.as().JoinedMembers(p.ctx, room)
	p.must(err, "joined members")
	p.check(slices.Contains(members, a.UserID), "agent is a member", "members %v", members)

	eid, err := own.Send(p.ctx, room, "f4-1", text("hello from f4"))
	p.check(err == nil && eid != "", "own-token send", "%s, event %q", describe(err), eid)
	if err == nil {
		ev, err := p.as().Event(p.ctx, room, eid)
		p.must(err, "fetch the sent event")
		p.check(ev.Sender == a.UserID && bodyOf(*ev) == "hello from f4", "sent event as the agent",
			"sender %q, body %q", ev.Sender, bodyOf(*ev))
	}
}

// F5: a passwordless agent.x.listener syncs with the 5.3 filter, sees messages, reads
// /event/{id}, relations and joined members.
func TestF5StandingSync(t *testing.T) {
	p := startProbe(t, "F5")
	if p == nil {
		return
	}
	room := p.agentsRoom()
	other := p.agentsRoom2()
	s := p.agent("agent.x.listener", "KWAK-f5")
	for _, r := range []string{room, other} {
		_, err := p.asUser(s.UserID).Join(p.ctx, r)
		p.must(err, "standing account joins")
	}
	alice := p.client(p.env.Alice.AccessToken)
	_, err := alice.Join(p.ctx, room)
	p.must(err, "alice joins")
	_, err = alice.Join(p.ctx, other)
	p.must(err, "alice joins the other room")

	sc := p.client(s.AccessToken)
	filter := syncFilter(room)
	first, err := sc.Sync(p.ctx, "", filter, 0)
	p.check(err == nil && first.NextBatch != "", "first sync with the 5.3 filter", "%s", describe(err))
	if err != nil {
		return
	}

	root, err := alice.Send(p.ctx, room, "f5-root", text("f5 root"))
	p.must(err, "alice sends the root")
	reply, err := alice.Send(p.ctx, room, "f5-reply", map[string]any{
		"msgtype": "m.text", "body": "f5 reply",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root, "is_falling_back": true,
			"m.in_reply_to": map[string]any{"event_id": root}},
	})
	p.must(err, "alice sends the thread reply")
	_, err = p.client(p.env.Bob.AccessToken).Join(p.ctx, room)
	p.must(err, "bob joins (a member event the filter drops)")
	stray, err := alice.Send(p.ctx, other, "f5-other", text("f5 other room"))
	p.must(err, "alice sends in the other room")
	last, err := alice.Send(p.ctx, room, "f5-last", text("f5 last"))
	p.must(err, "alice sends the last message")

	seen := map[string]matrix.Event{}
	var types []string
	since := first.NextBatch
	for i := 0; i < 20 && seen[last].EventID == ""; i++ {
		r, err := sc.Sync(p.ctx, since, filter, 5*time.Second)
		p.must(err, "incremental sync")
		since = r.NextBatch
		for rid, jr := range r.Rooms.Join {
			for _, ev := range jr.Timeline.Events {
				ev.RoomID = rid
				seen[ev.EventID] = ev
				types = append(types, ev.Type)
			}
		}
	}
	p.check(seen[root].EventID != "" && seen[reply].EventID != "" && seen[last].EventID != "",
		"messages arrive through sync", "root %v, reply %v, last %v", seen[root].EventID != "",
		seen[reply].EventID != "", seen[last].EventID != "")
	onlyMsgs := true
	for _, ty := range types {
		onlyMsgs = onlyMsgs && ty == "m.room.message"
	}
	p.check(onlyMsgs, "timeline holds only m.room.message", "types %v", types)
	_, strayed := seen[stray]
	p.check(!strayed, "room filter drops the other room", "other room event seen %v", strayed)

	ev, err := sc.Event(p.ctx, room, root)
	p.check(err == nil && ev.Sender == p.env.Alice.UserID && bodyOf(*ev) == "f5 root", "/event/{id}",
		"%s", describeEvent(ev, err))

	replies, err := sc.ThreadReplies(p.ctx, room, root, 50)
	ids := eventIDs(replies)
	p.check(err == nil && slices.Contains(ids, reply), "thread relations", "%s, %d replies, has reply %v",
		describe(err), len(ids), slices.Contains(ids, reply))

	members, err := sc.JoinedMembers(p.ctx, room)
	p.check(err == nil && slices.Contains(members, s.UserID) && slices.Contains(members, p.env.Alice.UserID) &&
		slices.Contains(members, p.env.Bob.UserID), "joined members", "%s, %v", describe(err), members)
}

// agentsRoom2 is a second public room, to check the 5.3 room filter.
func (p *probe) agentsRoom2() string {
	p.t.Helper()
	room, err := p.client(p.env.Admin.AccessToken).CreateRoom(p.ctx,
		map[string]any{"preset": "public_chat", "visibility": "private", "name": "other"})
	p.must(err, "create the other room")
	return room
}

// F6: a ?user_id=&device_id= logout ends one device; a new login on that device works;
// logout/all ends every device.
func TestF6DeviceLogout(t *testing.T) {
	p := startProbe(t, "F6")
	if p == nil {
		return
	}
	u := mxid("agent.f6.listener")
	a := p.agent("agent.f6.listener", "KWAK-a")
	b, err := p.as().LoginAppservice(p.ctx, u, "KWAK-b")
	p.must(err, "login KWAK-b")
	p.check(a.DeviceID == "KWAK-a" && b.DeviceID == "KWAK-b", "device ids as asked", "%q, %q", a.DeviceID, b.DeviceID)
	alive := func(tok string) (bool, string) {
		st, _, code := p.whoami(tok, "")
		return st == http.StatusOK, fmt.Sprintf("%d %s", st, code)
	}
	okA, sA := alive(a.AccessToken)
	okB, sB := alive(b.AccessToken)
	p.check(okA && okB, "both tokens work before", "KWAK-a %s, KWAK-b %s", sA, sB)

	dev := &matrix.Client{Base: p.env.Homeserver, Token: p.env.Registration.ASToken, AsUser: u, AsDevice: "KWAK-a",
		ForwardedFor: ittest.ForwardedFor, AllowHTTP: true, HTTP: p.env.HTTP}
	err = dev.Logout(p.ctx)
	p.check(err == nil, "?user_id=&device_id= logout", "%s", describe(err))
	okA, sA = alive(a.AccessToken)
	okB, sB = alive(b.AccessToken)
	okAS, sAS := alive(p.env.Registration.ASToken)
	p.check(!okA, "KWAK-a token dead", "whoami %s", sA)
	p.check(okB, "KWAK-b token survives", "whoami %s", sB)
	p.check(okAS, "as_token survives", "whoami %s", sAS)

	a2, err := p.as().LoginAppservice(p.ctx, u, "KWAK-a")
	p.check(err == nil && a2.DeviceID == "KWAK-a", "login again on KWAK-a", "%s, device %q", describe(err), a2.DeviceID)
	okA2, sA2 := alive(a2.AccessToken)
	okA, sA = alive(a.AccessToken)
	p.check(okA2 && !okA, "new token works, old stays dead", "new %s, old %s", sA2, sA)

	err = p.asUser(u).LogoutAll(p.ctx)
	p.check(err == nil, "?user_id= logout/all", "%s", describe(err))
	okA2, sA2 = alive(a2.AccessToken)
	okB, sB = alive(b.AccessToken)
	okAS, sAS = alive(p.env.Registration.ASToken)
	p.check(!okA2 && !okB, "logout/all kills every device", "KWAK-a %s, KWAK-b %s", sA2, sB)
	p.check(okAS, "as_token survives logout/all", "whoami %s", sAS)
}

// F7: a login on an existing device without a logout; does the old token survive? Recorded only.
func TestF7LoginSameDevice(t *testing.T) {
	p := startProbe(t, "F7")
	if p == nil {
		return
	}
	u := mxid("agent.f7.listener")
	first := p.agent("agent.f7.listener", "KWAK-c")
	second, err := p.as().LoginAppservice(p.ctx, u, "KWAK-c")
	if err != nil {
		p.note("second login on KWAK-c", "%s", describe(err))
		return
	}
	st1, _, c1 := p.whoami(first.AccessToken, "")
	st2, _, c2 := p.whoami(second.AccessToken, "")
	p.note("second login on KWAK-c", "device %q, same token %v", second.DeviceID, first.AccessToken == second.AccessToken)
	p.note("old token after the second login", "whoami %d %s (survives %v)", st1, c1, st1 == http.StatusOK)
	p.note("new token", "whoami %d %s", st2, c2)
}

// F8: setting the display name through ?user_id= replaces the default suffix.
func TestF8DisplayNameSuffix(t *testing.T) {
	p := startProbe(t, "F8")
	if p == nil {
		return
	}
	const lp = "agent.f8.0123456789abcdef"
	u := mxid(lp)
	p.agent(lp, "")
	tok := p.env.Registration.ASToken
	st, dn := p.profileField(tok, u, "displayname")
	p.note("display name after appservice /register", "status %d, %q", st, dn)

	err := p.asUser(u).SetDisplayName(p.ctx, u, lp)
	p.check(err == nil, "?user_id= set display name", "%s", describe(err))
	st, dn = p.profileField(tok, u, "displayname")
	p.check(dn == lp, "display name is the localpart exactly", "status %d, %q", st, dn)
}

// F9: the as_token asserting a human outside the namespace gets M_EXCLUSIVE.
func TestF9NamespaceAnchor(t *testing.T) {
	p := startProbe(t, "F9")
	if p == nil {
		return
	}
	h := p.env.RegisterHuman(t, "magent.x")
	tok := p.env.Registration.ASToken

	st, uid, code := p.whoami(tok, h.UserID)
	p.check(code == matrix.ErrExclusive, "whoami ?user_id=@magent.x", "status %d, user %q, errcode %q", st, uid, code)

	err := p.asUser(h.UserID).SetDisplayName(p.ctx, h.UserID, "taken over")
	p.check(err != nil && errcode(err) == matrix.ErrExclusive, "set display name ?user_id=@magent.x", "%s", describe(err))
	_, dn := p.profileField(p.env.Alice.AccessToken, h.UserID, "displayname")
	p.check(dn != "taken over", "the human's name is unchanged", "%q", dn)

	st, uid, code = p.whoami(tok, p.env.Alice.UserID)
	p.check(code == matrix.ErrExclusive, "whoami ?user_id=@alice", "status %d, user %q, errcode %q", st, uid, code)

	_, err = p.as().RegisterAppservice(p.ctx, "magent.y", "")
	p.check(errcode(err) == matrix.ErrExclusive, "appservice /register of magent.y", "%s", describe(err))

	p.agent("agent.f9.0123456789abcdef", "")
	st, uid, code = p.whoami(tok, mxid("agent.f9.0123456789abcdef"))
	p.check(st == http.StatusOK && uid == mxid("agent.f9.0123456789abcdef"), "control: an agent in the namespace",
		"status %d, user %q, errcode %q", st, uid, code)
}

// F10: 150 joins between two messages give limited:true; /messages back-fill finds both.
func TestF10GapFill(t *testing.T) {
	p := startProbe(t, "F10")
	if p == nil {
		return
	}
	room := p.agentsRoom()
	s := p.agent("agent.x.listener", "KWAK-f10")
	_, err := p.asUser(s.UserID).Join(p.ctx, room)
	p.must(err, "standing account joins")
	alice := p.client(p.env.Alice.AccessToken)
	_, err = alice.Join(p.ctx, room)
	p.must(err, "alice joins")

	sc := p.client(s.AccessToken)
	filter := syncFilter(room)
	first, err := sc.Sync(p.ctx, "", filter, 0)
	p.must(err, "first sync")

	msg1, err := alice.Send(p.ctx, room, "f10-1", text("f10 first"))
	p.must(err, "msg1")
	const joins = 150
	var wg sync.WaitGroup
	errs := make(chan error, joins)
	sem := make(chan struct{}, 8)
	for i := range joins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			lp := fmt.Sprintf("agent.bulk.%016x", i)
			if _, err := p.as().RegisterAppservice(p.ctx, lp, ""); err != nil {
				errs <- fmt.Errorf("register %s: %s", lp, describe(err))
				return
			}
			if _, err := p.asUser(mxid(lp)).Join(p.ctx, room); err != nil {
				errs <- fmt.Errorf("join %s: %s", lp, describe(err))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		p.must(err, "bulk join")
	}
	msg2, err := alice.Send(p.ctx, room, "f10-2", text("f10 second"))
	p.must(err, "msg2")

	r, err := sc.Sync(p.ctx, first.NextBatch, filter, 5*time.Second)
	p.must(err, "sync since")
	tl := r.Rooms.Join[room].Timeline
	tlIDs := eventIDs(tl.Events)
	p.check(tl.Limited, "limited after 150 joins", "limited %v, %d timeline events, msg1 %v, msg2 %v, prev_batch set %v",
		tl.Limited, len(tlIDs), slices.Contains(tlIDs, msg1), slices.Contains(tlIDs, msg2), tl.PrevBatch != "")

	found := map[string]bool{}
	for _, id := range tlIDs {
		found[id] = true
	}
	from, pages := tl.PrevBatch, 0
	mf := `{"types":["m.room.message"]}`
	for !found[msg1] && pages < 50 {
		page, err := sc.Messages(p.ctx, room, from, 100, mf)
		p.must(err, "back-fill page")
		pages++
		for _, ev := range page.Chunk {
			found[ev.EventID] = true
		}
		if page.End == "" {
			break
		}
		from = page.End
	}
	p.check(found[msg1] && found[msg2], "back-fill finds both", "msg1 %v, msg2 %v, pages %d", found[msg1], found[msg2], pages)
}

// F11: a DM made by the as_token with the 3.5 body works in a v12 room; it is unencrypted; the
// human cannot send m.room.encryption; the human's message arrives in a transaction; FetchEvent
// matches.
func TestF11RegistrarDM(t *testing.T) {
	p := startProbe(t, "F11")
	if p == nil {
		return
	}
	rc := p.serveRecorder()
	tok := p.env.Registration.ASToken
	room, err := p.as().CreateRoom(p.ctx, dmBody(p.env.Alice.UserID))
	p.check(err == nil, "create the DM with the 3.5 body", "%s", describe(err))
	if err != nil {
		return
	}
	_, create := p.state(tok, room, "m.room.create", "")
	p.check(create["room_version"] == "12", "room version", "%v", create["room_version"])
	_, pl := p.state(tok, room, "m.room.power_levels", "")
	p.note("DM power levels", "%s", jsonString(pl))

	enc, err := p.as().RoomEncrypted(p.ctx, room)
	p.check(err == nil && !enc, "unencrypted (RoomEncrypted, no trailing slash)", "encrypted %v, %s", enc, describe(err))

	alice := p.client(p.env.Alice.AccessToken)
	_, err = alice.Join(p.ctx, room)
	p.must(err, "alice accepts the invite")
	members, err := p.as().JoinedMembers(p.ctx, room)
	p.must(err, "joined members")
	p.note("DM members", "%v", members)

	st, b := p.raw(http.MethodPut, v3+"/rooms"+esc(room, "state", "m.room.encryption", ""), p.env.Alice.AccessToken, true,
		map[string]any{"algorithm": "m.megolm.v1.aes-sha2"})
	p.check(st == http.StatusForbidden, "alice cannot send m.room.encryption", "status %d: %s", st, truncate(b))
	enc, err = p.as().RoomEncrypted(p.ctx, room)
	p.check(err == nil && !enc, "still unencrypted", "encrypted %v, %s", enc, describe(err))

	const cmd = "pair ABCD-1234"
	eid, err := alice.Send(p.ctx, room, "f11-1", text(cmd))
	p.must(err, "alice sends a command")
	got, ok := rc.waitEvent(p.ctx, 15*time.Second, func(ev matrix.Event) bool { return ev.EventID == eid })
	p.check(ok && got.Sender == p.env.Alice.UserID && bodyOf(got) == cmd, "message arrives in a transaction",
		"seen %v, sender %q, body %q", ok, got.Sender, bodyOf(got))

	fetched, err := p.as().Event(p.ctx, room, eid)
	p.check(err == nil && fetched.Sender == got.Sender && bodyOf(*fetched) == bodyOf(got), "FetchEvent matches",
		"%s", describeEvent(fetched, err))

	if hits := rc.transactionHits(); len(hits) > 0 {
		h := hits[0]
		p.note("transaction auth", "Authorization Bearer hs_token: present %v, matches %v; ?access_token: present %v, matches %v",
			h.BearerPresent, h.BearerMatches, h.QueryPresent, h.QueryMatches)
	}

	// Control: RoomEncrypted must say true for an encrypted room, or the false above means nothing.
	encRoom, err := alice.CreateRoom(p.ctx, map[string]any{"preset": "private_chat", "invite": []string{mxid("registrar")},
		"initial_state": []any{map[string]any{"type": "m.room.encryption", "state_key": "",
			"content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}}}})
	p.must(err, "alice creates an encrypted room")
	_, err = p.as().Join(p.ctx, encRoom)
	p.must(err, "registrar joins the encrypted room")
	enc, err = p.as().RoomEncrypted(p.ctx, encRoom)
	p.check(err == nil && enc, "control: RoomEncrypted sees an encrypted room", "encrypted %v, %s", enc, describe(err))
}

// F12: admin creates the room with the 3.11 body, giving power 50 to a @registrar who is not yet
// a member.
func TestF12RoomBootstrap(t *testing.T) {
	p := startProbe(t, "F12")
	if p == nil {
		return
	}
	admin := p.env.Admin.AccessToken
	room, err := p.client(admin).CreateRoom(p.ctx, agentsRoomBody())
	p.check(err == nil, "create with the 3.11 body", "%s", describe(err))
	if err != nil {
		return
	}
	_, create := p.state(admin, room, "m.room.create", "")
	p.note("create event", "room_version %v, content %s", create["room_version"], jsonString(create))
	_, pl := p.state(admin, room, "m.room.power_levels", "")
	users, _ := pl["users"].(map[string]any)
	p.check(users[mxid("registrar")] == float64(50), "registrar has 50 before joining", "power levels %s", jsonString(pl))
	_, hv := p.state(admin, room, "m.room.history_visibility", "")
	_, jr := p.state(admin, room, "m.room.join_rules", "")
	gst, _ := p.state(admin, room, "m.room.guest_access", "")
	est, _ := p.state(admin, room, "m.room.encryption", "")
	p.check(hv["history_visibility"] == "shared" && jr["join_rule"] == "public" && gst == http.StatusNotFound &&
		est == http.StatusNotFound, "room state as 3.11 says",
		"history %v, join rule %v, guest_access status %d, encryption status %d", hv["history_visibility"], jr["join_rule"], gst, est)

	alias := "#agents:" + ittest.ServerName
	rid, err := p.as().ResolveAlias(p.ctx, alias)
	p.check(err == nil && rid == room, "registrar resolves the alias", "%s, %q", describe(err), rid)
	rid, err = p.as().Join(p.ctx, alias)
	p.check(err == nil && rid == room, "registrar joins by alias", "%s", describe(err))

	_, pl = p.state(admin, room, "m.room.power_levels", "")
	users, _ = pl["users"].(map[string]any)
	p.check(users[mxid("registrar")] == float64(50), "registrar keeps 50 after joining", "users %v", users)

	a := p.agent("agent.f12.0123456789abcdef", "")
	_, err = p.asUser(a.UserID).Join(p.ctx, room)
	p.must(err, "agent joins")
	st, b := p.raw(http.MethodPost, v3+"/rooms"+esc(room, "kick"), p.env.Registration.ASToken, true,
		map[string]any{"user_id": a.UserID, "reason": "f12"})
	p.check(st == http.StatusOK, "registrar at 50 can kick", "status %d: %s", st, truncate(b))
	st, b = p.raw(http.MethodPut, v3+"/rooms"+esc(room, "state", "m.room.topic", ""), p.env.Registration.ASToken, true,
		map[string]any{"topic": "changed"})
	p.check(st == http.StatusForbidden, "registrar at 50 cannot change the topic", "status %d: %s", st, truncate(b))
}

// F13: an agent changes its own display name; the registrar's reset through ?user_id= restores it.
func TestF13DisplayNameReset(t *testing.T) {
	p := startProbe(t, "F13")
	if p == nil {
		return
	}
	rc := p.serveRecorder()
	room := p.agentsRoom()
	_, err := p.as().Join(p.ctx, room)
	p.must(err, "registrar joins")

	const lp = "agent.alice.0123456789abcdef"
	a := p.agent(lp, "")
	u := a.UserID
	p.must(p.asUser(u).SetDisplayName(p.ctx, u, lp), "registrar names the agent")
	_, err = p.asUser(u).Join(p.ctx, room)
	p.must(err, "agent joins")
	own := p.client(a.AccessToken)
	tok := p.env.Registration.ASToken

	err = own.SetDisplayName(p.ctx, u, "alice")
	p.note("agent sets its own display name", "%s", describe(err))
	err2 := own.SetAvatarURL(p.ctx, u, "mxc://"+ittest.ServerName+"/f13avatar")
	p.note("agent sets its own avatar", "%s", describe(err2))
	if err == nil {
		ev, ok := rc.waitEvent(p.ctx, 15*time.Second, func(ev matrix.Event) bool {
			return ev.Type == "m.room.member" && ev.Sender == u && strings.Contains(string(ev.Content), `"alice"`)
		})
		p.check(ok, "the member event reaches the registrar", "seen %v, content %s", ok, string(ev.Content))
		_, m := p.state(tok, room, "m.room.member", u)
		p.note("room member state after the agent's change", "displayname %v, avatar_url %v", m["displayname"], m["avatar_url"])
	} else {
		p.note("premise", "the agent cannot rename itself, so the reset is a second line only")
		p.must(p.asUser(u).SetDisplayName(p.ctx, u, "alice"), "set a foreign name through ?user_id= to test the reset")
		p.must(p.asUser(u).SetAvatarURL(p.ctx, u, "mxc://"+ittest.ServerName+"/f13avatar"), "set an avatar through ?user_id=")
	}

	err = p.asUser(u).SetDisplayName(p.ctx, u, lp)
	p.check(err == nil, "reset display name through ?user_id=", "%s", describe(err))
	err = p.asUser(u).SetAvatarURL(p.ctx, u, "")
	p.check(err == nil, "clear avatar through ?user_id=", "%s", describe(err))

	_, dn := p.profileField(tok, u, "displayname")
	st, av := p.profileField(tok, u, "avatar_url")
	p.check(dn == lp, "profile display name restored", "%q", dn)
	p.check(av == "", "profile avatar cleared", "status %d, %q", st, av)
	_, m := p.state(tok, room, "m.room.member", u)
	p.check(m["displayname"] == lp && (m["avatar_url"] == nil || m["avatar_url"] == ""), "room member state restored",
		"displayname %v, avatar_url %v", m["displayname"], m["avatar_url"])
}

func eventIDs(evs []matrix.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.EventID)
	}
	return out
}

func describeEvent(ev *matrix.Event, err error) string {
	if err != nil {
		return describe(err)
	}
	return fmt.Sprintf("sender %q, type %q, body %q", ev.Sender, ev.Type, bodyOf(*ev))
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// F14 (decision 42): a listener that moves to a new account keeps its next_batch (spec 5.3). The
// new account joined the room after that since token. Its first sync from it must not replay what
// came before since, and must hold what came after: the message posted between the switch and the
// join (visible under history_visibility shared) and the one after the join.
func TestF14NewAccountKeepsSince(t *testing.T) {
	p := startProbe(t, "F14")
	if p == nil {
		return
	}
	room := p.agentsRoom()
	old := p.agent("agent.f14.listener", "KWAK-f14")
	_, err := p.asUser(old.UserID).Join(p.ctx, room)
	p.must(err, "old account joins")
	alice := p.client(p.env.Alice.AccessToken)
	_, err = alice.Join(p.ctx, room)
	p.must(err, "alice joins")

	filter := syncFilter(room)
	oc := p.client(old.AccessToken)
	first, err := oc.Sync(p.ctx, "", filter, 0)
	p.must(err, "old account's first sync")
	before, err := alice.Send(p.ctx, room, "f14-before", text("f14 before"))
	p.must(err, "the message the old account routes")
	since := first.NextBatch
	for seen := false; !seen; {
		r, err := oc.Sync(p.ctx, since, filter, 5*time.Second)
		p.must(err, "old account's sync")
		since = r.NextBatch
		seen = slices.Contains(eventIDs(r.Rooms.Join[room].Timeline.Events), before)
	}

	gap, err := alice.Send(p.ctx, room, "f14-gap", text("f14 gap"))
	p.must(err, "the message between the switch and the join")
	nu := p.agent("agent.f14.brave-heron.listener", "KWAK-f14")
	_, err = p.asUser(nu.UserID).Join(p.ctx, room)
	p.must(err, "new account joins")
	after, err := alice.Send(p.ctx, room, "f14-after", text("f14 after"))
	p.must(err, "the message after the join")

	nc := p.client(nu.AccessToken)
	var ids []string
	limited, prev, next := false, "", since
	for i := 0; i < 20 && !slices.Contains(ids, after); i++ {
		r, err := nc.Sync(p.ctx, next, filter, 5*time.Second)
		p.must(err, "new account's sync from the old since")
		next = r.NextBatch
		tl := r.Rooms.Join[room].Timeline
		if tl.Limited && prev == "" {
			limited, prev = true, tl.PrevBatch
		}
		ids = append(ids, eventIDs(tl.Events)...)
	}
	p.check(!slices.Contains(ids, before), "no replay before since", "timeline %v, before %v", ids, before)
	p.check(slices.Contains(ids, gap) && slices.Contains(ids, after), "the gap and the later message arrive",
		"gap %v, after %v, %d events", slices.Contains(ids, gap), slices.Contains(ids, after), len(ids))
	p.note("limited for a room joined after since", "limited %v, prev_batch set %v", limited, prev != "")
	if !limited {
		return
	}
	// A limited timeline makes the listener page back to last_event_id (5.3), here the message the
	// old account routed, from before the new account's join.
	found, pages, from := false, 0, prev
	for !found && from != "" && pages < 10 {
		page, err := nc.Messages(p.ctx, room, from, 100, `{"types":["m.room.message"]}`)
		p.must(err, "back-fill page of the new account")
		pages++
		found = slices.Contains(eventIDs(page.Chunk), before)
		from = page.End
	}
	p.check(found, "the gap fill reaches last_event_id from before the join", "found %v, pages %d", found, pages)
}
