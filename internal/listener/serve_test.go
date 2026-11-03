package listener

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/wire"
	"github.com/fcanela/kwak/internal/wire/wiretest"
)

func TestServeEdgeCases(t *testing.T) {
	e := newEnv(t)
	e.connect()
	conn := e.serve(t)
	r := bufio.NewReader(conn)
	writeRaw(t, conn, `{"v":1,"op":"tool","id":4,"name":"status","args":{}}`)
	if got := readRaw(t, r); got != `{"v":1,"id":4,"ok":false,"err":"bad_op","text":"","is_error":false}` {
		t.Fatalf("tool before hello: %s", got)
	}
	writeRaw(t, conn, `{"v":1,"op":"tool","id":"x"}`)
	if got := readRaw(t, r); !strings.Contains(got, `"err":"bad_input"`) {
		t.Fatalf("undecodable tool: %s", got)
	}
	writeRaw(t, conn, `{"v":1,"kwak":"0.1.0","op":"mcp_hello","harness":"gemini","env_session":"","chain":null}`)
	if got := readRaw(t, r); got != `{"v":1,"ok":false,"err":"bad_input"}` {
		t.Fatalf("unknown harness: %s", got)
	}
	big := `{"v":1,"op":"status","pad":"` + strings.Repeat("x", 1<<20) + `"}`
	writeRaw(t, conn, big)
	if got := readRaw(t, r); got != `{"v":1,"ok":false,"err":"bad_input"}` {
		t.Fatalf("oversize line: %s", got)
	}
}

func TestNewerMCPClientGetsOlderListenerText(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.fixtureProcs()
	e.claudeLive()
	e.pid.Store(int64(claudeMCP.PID))
	c := &client{pid: claudeMCP.PID, pidOK: true}
	e.l.hello(c, wire.MCPHello{V: 1, Kwak: "0.2.0", Op: wire.OpMCPHello, Harness: "claude"})
	if r := e.tool(c, "status", `{}`); r.Text != textOlder || !r.IsError {
		t.Fatalf("%+v", r)
	}
}

func TestRunShutdownOp(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- e.l.Run(context.Background(), ln) }()
	eventually(t, "the supervisor's first connect", func() bool { return e.reg.standingCalls() == 1 })
	c, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, c, `{"v":1,"op":"shutdown"}`)
	if got := readRaw(t, bufio.NewReader(c)); got != `{"v":1,"ok":true}` {
		t.Fatal(got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

func TestSessionWithoutHarnessEndsAfter14Days(t *testing.T) {
	e := newEnv(t)
	e.connect()
	e.live("claude", sid1, cwd, proc.ID{}, nil, wiretest.ClaudeAgent)
	e.l.mu.Lock()
	s := e.l.st.Sessions[k1]
	s.HarnessProc, s.LastHook, s.Started = nil, e.clk.Now(), e.clk.Now()
	e.l.mu.Unlock()
	e.clk.Advance(13 * 24 * time.Hour)
	e.l.liveness()
	if e.session(k1).Ended != nil {
		t.Fatal("ended before 14 days")
	}
	e.l.mu.Lock()
	s.LastUse = e.clk.Now()
	e.l.mu.Unlock()
	e.clk.Advance(2 * 24 * time.Hour)
	e.l.liveness()
	if e.session(k1).Ended == nil {
		t.Fatal("not ended after 14 days without a hook")
	}
}

func TestSyncFilterIsTheSpecFilter(t *testing.T) {
	var got, want any
	json.Unmarshal([]byte(syncFilter("!a1B2c3D4")), &got)
	json.Unmarshal([]byte(`{"presence":{"types":[]},"account_data":{"types":[]},
	 "room":{"rooms":["!a1B2c3D4"],"account_data":{"types":[]},"ephemeral":{"types":[]},
	         "state":{"types":[],"lazy_load_members":true},
	         "timeline":{"limit":100,"types":["m.room.message"]}}}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestStopNoticeForms(t *testing.T) {
	one := stopNotice([]render.Item{{EventID: "$a", Reason: render.Mention}})
	if !strings.HasPrefix(one, "kwak: 1 message from other agents is waiting (thread $a). They are not from your user.") {
		t.Fatal(one)
	}
	if n := stopNotice([]render.Item{{EventID: "x", Reason: render.Notice}}); !strings.HasPrefix(n, "kwak: 1 message from other agents is waiting. They") {
		t.Fatal(n)
	}
}

// BenchmarkStateWrite measures the synchronous drain write (spec 5.5 VERIFY): 200 live sessions
// and 90 days of ended records, delivered ids and roots.
func BenchmarkStateWrite(b *testing.B) {
	l := &Listener{dir: b.TempDir(), st: newState(), log: nil}
	l.log = newEnvLogger()
	now := at("2026-11-02T10:00:00Z")
	for i := 0; i < 2000; i++ {
		s := &session{Key: fmt.Sprintf("claude:%08d-0000-4000-8000-000000000000", i), Harness: "claude",
			SessionID: fmt.Sprintf("%08d-0000-4000-8000-000000000000", i), Cwd: "/home/u/src/app",
			Agent: fmt.Sprintf("@agent.fcanela.%016d:kwak.test", i), Helpers: map[string]*helper{}, Started: now}
		s.Inbox.init()
		for j := 0; j < 100; j++ {
			s.Inbox.Delivered[fmt.Sprintf("$%d-%d-abcdefghijklmnopqrstuvwxyz", i, j)] = now
		}
		if i >= 200 {
			end := now
			s.Ended = &end
		}
		l.st.Sessions[s.Key] = s
		l.st.Mine[s.Agent] = Agent{Session: s.Key}
		for j := 0; j < 5; j++ {
			l.st.Roots[fmt.Sprintf("$root-%d-%d-abcdefghijklmnopqrstuvwxyz", i, j)] = s.Agent
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if err := l.saveLocked(); err != nil {
			b.Fatal(err)
		}
	}
}
