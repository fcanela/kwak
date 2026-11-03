package listener

import (
	"reflect"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

const (
	agA  = "@agent.fcanela.aaaaaaaaaaaaaaaa:kwak.test"
	agB  = "@agent.fcanela.bbbbbbbbbbbbbbbb:kwak.test"
	agAh = "@agent.fcanela.aaaaaaaaaaaaaaaa.tests:kwak.test"
)

func testIndex() *Index {
	return &Index{
		Room: wiretest.RoomID,
		Mine: map[string]Agent{
			agA:  {Session: "claude:a"},
			agB:  {Session: "claude:b"},
			agAh: {Session: "claude:a", Helper: "tests"},
		},
		Roots:      map[string]string{"$rootA": agA, "$rootB": agB, "$rootH": agAh},
		Threads:    map[string]Thread{},
		Volunteers: map[string]string{},
	}
}

func d(uid string, r render.Reason) Delivery {
	a := testIndex().Mine[uid]
	return Delivery{UserID: uid, Session: a.Session, Helper: a.Helper, Reason: r}
}

func TestRoute(t *testing.T) {
	ts := at("2026-11-02T10:00:00Z")
	edit := text("fixed", agA)
	edit["m.relates_to"] = map[string]any{"rel_type": "m.replace", "event_id": "$x"}
	emote := text("waves", agA)
	emote["msgtype"] = "m.emote"
	badBody := map[string]any{"msgtype": "m.text", "body": 7, "m.mentions": map[string]any{"user_ids": []string{agA}}}
	roomPing := map[string]any{"msgtype": "m.text", "body": "@room hello", "m.mentions": map[string]any{"room": true}}
	notice := text("ping", agA)
	notice["msgtype"] = "m.notice"
	other := event("$o", bob, ts, text("hi", agA))
	other.RoomID = "!other"
	member := event("$m", bob, ts, text("hi", agA))
	member.Type = "m.room.member"
	cases := []struct {
		name string
		ev   matrix.Event
		vol  map[string]string
		want []Delivery
	}{
		{"mention", event("$1", bob, ts, text("q", agA)), nil, []Delivery{d(agA, render.Mention)}},
		{"notice mention", event("$1", bob, ts, notice), nil, []Delivery{d(agA, render.Mention)}},
		{"reply in our thread, no mention", event("$2", bob, ts, inThread(text("a"), "$rootA")), nil, []Delivery{d(agA, render.YourThread)}},
		{"both give one mention", event("$3", bob, ts, inThread(text("a", agA), "$rootA")), nil, []Delivery{d(agA, render.Mention)}},
		{"reply in a foreign thread", event("$4", bob, ts, inThread(text("a"), "$foreign")), nil, nil},
		{"A mentions B", event("$5", agA, ts, text("q", agB)), nil, []Delivery{d(agB, render.Mention)}},
		{"B replies in A's thread", event("$6", agB, ts, inThread(text("a"), "$rootA")), nil, []Delivery{d(agA, render.YourThread)}},
		{"helper mentions its parent", event("$7", agAh, ts, text("q", agA)), nil, []Delivery{d(agA, render.Mention)}},
		{"parent replies in its helper's thread", event("$8", agA, ts, inThread(text("a"), "$rootH")), nil, []Delivery{d(agAh, render.YourThread)}},
		{"own reply in own thread", event("$9", agA, ts, inThread(text("a"), "$rootA")), nil, nil},
		{"self mention", event("$10", agA, ts, text("me", agA)), nil, nil},
		{"open question, no volunteers", event("$11", bob, ts, text("anyone?")), nil, nil},
		{"open question, volunteers", event("$12", bob, ts, text("anyone?")), map[string]string{"claude:a": agA, "claude:b": agB},
			[]Delivery{d(agA, render.OpenQuestion), d(agB, render.OpenQuestion)}},
		{"asking volunteer skips itself", event("$13", agA, ts, text("anyone?")), map[string]string{"claude:a": agA, "claude:b": agB},
			[]Delivery{d(agB, render.OpenQuestion)}},
		{"mentioned volunteer gets a mention", event("$14", bob, ts, text("you?", agB)), map[string]string{"claude:b": agB},
			[]Delivery{d(agB, render.Mention)}},
		{"edit dropped", event("$15", bob, ts, edit), nil, nil},
		{"emote dropped", event("$16", bob, ts, emote), nil, nil},
		{"body not a string", event("$17", bob, ts, badBody), nil, nil},
		{"other room", other, nil, nil},
		{"not a message", member, nil, nil},
		{"@room is not a mention", event("$18", bob, ts, roomPing), nil, nil},
		{"plain-text @name ignored", event("$19", bob, ts, text("hey @agent.fcanela.aaaaaaaaaaaaaaaa:kwak.test")), nil, nil},
		{"mention of a foreign agent", event("$20", bob, ts, text("q", "@agent.zed.0000000000000000:kwak.test")), nil, nil},
	}
	for _, c := range cases {
		idx := testIndex()
		if c.vol != nil {
			idx.Volunteers = c.vol
		}
		got := Route(c.ev, idx)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestRouteRecordsRootsAndThreads(t *testing.T) {
	idx := testIndex()
	ts := at("2026-11-02T10:00:00Z")
	if got := Route(event("$q", agA, ts, text("question", bob)), idx); len(got) != 0 {
		t.Fatalf("own echo delivered: %+v", got)
	}
	if idx.Roots["$q"] != agA {
		t.Fatalf("root not recorded: %v", idx.Roots)
	}
	Route(event("$r", bob, ts.Add(time.Second), inThread(text("a"), "$q")), idx)
	if idx.Threads["$q"].Latest != "$r" {
		t.Fatalf("thread latest %+v", idx.Threads["$q"])
	}
	Route(event("$old", bob, ts, inThread(text("a"), "$q")), idx)
	if idx.Threads["$q"].Latest != "$r" {
		t.Fatalf("an older event replaced the latest: %+v", idx.Threads["$q"])
	}
	Route(event("$x", bob, ts, text("not a root of ours")), idx)
	if _, ok := idx.Roots["$x"]; ok {
		t.Fatal("foreign root recorded")
	}
}

func TestRouteRootFallbackAfterReset(t *testing.T) {
	idx := testIndex()
	idx.Roots = map[string]string{}
	ev := event("$r", bob, at("2026-11-02T10:00:00Z"), inThread(text("a"), "$lost"))
	if got := Route(ev, idx); len(got) != 0 || len(idx.Missing) != 0 {
		t.Fatalf("complete index: got %+v, missing %v", got, idx.Missing)
	}
	idx.Incomplete = true
	Route(ev, idx)
	Route(ev, idx)
	if !reflect.DeepEqual(idx.Missing, []string{"$lost"}) {
		t.Fatalf("missing %v, want one fetch", idx.Missing)
	}
	idx.Roots["$lost"] = agB
	if got := Route(ev, idx); !reflect.DeepEqual(got, []Delivery{d(agB, render.YourThread)}) {
		t.Fatalf("after fetch: %+v", got)
	}
}

func TestRootFetchedOnceThroughSync(t *testing.T) {
	e := newEnv(t)
	e.connect()
	s := e.live("claude", sid1, wiretest.ClaudeCwd, claudeH, nil, agA)
	e.l.idx.Incomplete = true
	e.room.events["$lost"] = event("$lost", agA, at("2026-11-02T09:00:00Z"), text("q", bob))
	e.deliver(event("$r1", bob, at("2026-11-02T10:00:00Z"), inThread(text("a"), "$lost")),
		event("$r2", bob, at("2026-11-02T10:01:00Z"), inThread(text("b"), "$lost")))
	if n := len(e.items(s.Key)); n != 2 || e.room.fetches != 1 {
		t.Fatalf("items %d, fetches %d", n, e.room.fetches)
	}
}
