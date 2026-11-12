package registrar

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

func TestStoreAtomicWriteAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenStore wrote a file before any Update: %v", err)
	}
	err = s.Update(func(st *State) error {
		st.RoomID = "!r"
		st.putAgent("agent.o.0123456789abcdef", &Agent{Owner: "o", Kind: wire.KindSession, Harness: wire.HarnessClaude,
			Session: "0123456789abcdef0123456789abcdef", Created: t0})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("temp files left: %v", ents)
	}
	var raw map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &raw); err != nil || raw["version"] != float64(1) || raw["room_id"] != "!r" {
		t.Fatalf("file: %v %s", err, data)
	}
	for _, k := range []string{"owners", "dms", "pairings", "agents", "done_events"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("state.json has no %q", k)
		}
	}

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.View(func(st *State) {
		lp, a := st.lookup(sessionKey{"o", wire.HarnessClaude, "0123456789abcdef0123456789abcdef", ""})
		if st.RoomID != "!r" || a == nil || lp != "agent.o.0123456789abcdef" {
			t.Errorf("reload: room %q, session map %q %v", st.RoomID, lp, a)
		}
	})
}

func TestStoreRefusesCorruptOrNewerFile(t *testing.T) {
	for name, content := range map[string]string{
		"not json":      `{"version":1,`,
		"newer version": `{"version":2}`,
		"unknown field": `{"version":1,"surprise":true}`,
		"empty":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			os.WriteFile(path, []byte(content), 0o600)
			if _, err := OpenStore(path); err == nil {
				t.Fatal("OpenStore accepted it")
			}
			got, _ := os.ReadFile(path)
			if string(got) != content {
				t.Fatal("the file was changed")
			}
		})
	}
}

// A state.json written before decision 44 holds the owner's background resume setting. It still
// loads under the strict decoder, the value is dropped, and the next write leaves it out.
func TestStoreDropsTheOldResumeSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"version":1,"room_id":"!r","owners":{"fcanela":{"human":"@fcanela:kwak.test","alias":false,` +
		`"added":"2026-11-03T10:00:00Z","added_by":"@admin:kwak.test","removed":null,` +
		`"settings":{"resume":"all","question_timeout":"20m"}}},"dms":{},"pairings":{},"agents":{},"done_events":[]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("an older state.json with settings.resume: %v", err)
	}
	s.View(func(st *State) {
		if got := st.Owners["fcanela"].Settings; got != (OwnerSettings{QuestionTimeout: "20m"}) {
			t.Errorf("settings %+v", got)
		}
	})
	if err := s.Update(func(st *State) error { st.RoomID = "!r2"; return nil }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"resume"`) || !strings.Contains(string(data), `"20m"`) {
		t.Fatalf("written: %s", data)
	}
}

func TestStoreRollsBackAFailedUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenStore(path)
	s.Update(func(st *State) error { st.RoomID = "!kept"; return nil })
	err := s.Update(func(st *State) error {
		st.RoomID = "!lost"
		return errors.New("no")
	})
	if err == nil {
		t.Fatal("no error")
	}
	s.View(func(st *State) {
		if st.RoomID != "!kept" {
			t.Errorf("room %q after a failed update", st.RoomID)
		}
	})
	os.Chmod(filepath.Dir(path), 0o500)
	defer os.Chmod(filepath.Dir(path), 0o700)
	if os.Getuid() != 0 {
		if err := s.Update(func(st *State) error { st.RoomID = "!unwritable"; return nil }); err == nil {
			t.Fatal("write into a read-only dir succeeded")
		}
		s.View(func(st *State) {
			if st.RoomID != "!kept" {
				t.Errorf("room %q after a failed write", st.RoomID)
			}
		})
	}
}

func TestStoreTouchIsMemoryUntilFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenStore(path)
	lp := "agent.o.0123456789abcdef"
	s.Update(func(st *State) error {
		st.putAgent(lp, &Agent{Owner: "o", Kind: wire.KindSession, Session: strings.Repeat("0", 32), Created: t0, LastActive: t0})
		return nil
	})
	later := t0.Add(time.Hour)
	s.Touch(lp, later)
	s.Touch(lp, t0.Add(time.Minute))
	if st, _ := OpenStore(path); st != nil {
		st.View(func(st *State) {
			if !st.Agents[lp].LastActive.Equal(t0) {
				t.Error("Touch reached the disk before a Flush")
			}
		})
	}
	s.View(func(st *State) {
		if !st.Agents[lp].LastActive.Equal(later) {
			t.Errorf("in memory %v, want %v (Touch never moves back)", st.Agents[lp].LastActive, later)
		}
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	st, _ := OpenStore(path)
	st.View(func(st *State) {
		if !st.Agents[lp].LastActive.Equal(later) {
			t.Errorf("after Flush %v", st.Agents[lp].LastActive)
		}
	})
}

func TestDoneEventsKeepsTheLast512(t *testing.T) {
	st := newState()
	id := func(i int) string {
		return "$e" + strings.Repeat("x", i%7) + string(rune('A'+i%26)) + time.Duration(i).String()
	}
	for i := range 600 {
		st.markDone(id(i))
	}
	if len(st.DoneEvents) != maxDoneEvents || !st.done(id(599)) || !st.done(id(88)) || st.done(id(87)) {
		t.Fatalf("%d kept; last %v, 88 %v, 87 %v", len(st.DoneEvents), st.done(id(599)), st.done(id(88)), st.done(id(87)))
	}
}
