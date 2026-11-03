package proc_test

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/proc/proctest"
)

var (
	hook    = proc.ID{PID: 300, Start: 30}
	shell   = proc.ID{PID: 200, Start: 20}
	harness = proc.ID{PID: 100, Start: 10}
	initp   = proc.ID{PID: 1, Start: 1}
)

func TestChain(t *testing.T) {
	r := proctest.New()
	r.Line(hook, shell, harness, initp)

	cases := []struct {
		name string
		max  int
		want []proc.ID
	}{
		{"full", 8, []proc.ID{hook, shell, harness, initp}},
		{"capped", 2, []proc.ID{hook, shell}},
		{"one", 1, []proc.ID{hook}},
		{"zero", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := proc.Chain(r, hook.PID, c.max)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Chain = %v, want %v", got, c.want)
			}
		})
	}
}

func TestChainStops(t *testing.T) {
	t.Run("unknown start pid", func(t *testing.T) {
		if _, err := proc.Chain(proctest.New(), 42, 8); !errors.Is(err, proc.ErrNoProcess) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ancestor gone", func(t *testing.T) {
		r := proctest.New()
		r.Line(hook, shell, harness)
		r.Kill(shell.PID)
		want(t, r, []proc.ID{hook})
	})
	t.Run("ancestor unreadable", func(t *testing.T) {
		r := proctest.New()
		r.Line(hook, shell, harness)
		r.Fail(harness.PID, errors.New("permission denied"))
		want(t, r, []proc.ID{hook, shell})
	})
	t.Run("cycle", func(t *testing.T) {
		r := proctest.New()
		r.Add(hook, shell)
		r.Add(shell, hook)
		want(t, r, []proc.ID{hook, shell})
	})
	t.Run("own parent", func(t *testing.T) {
		r := proctest.New()
		r.Add(hook, hook)
		want(t, r, []proc.ID{hook})
	})
	t.Run("parent pid reused", func(t *testing.T) {
		r := proctest.New()
		r.Add(hook, shell)
		r.Add(proc.ID{PID: shell.PID, Start: 99}, harness)
		r.Add(harness, proc.ID{})
		want(t, r, []proc.ID{hook})
	})
	t.Run("parent start unknown", func(t *testing.T) {
		r := proctest.New()
		r.Add(hook, proc.ID{PID: shell.PID})
		r.Add(shell, proc.ID{})
		want(t, r, []proc.ID{hook, shell})
	})
}

func want(t *testing.T, r proc.Reader, w []proc.ID) {
	t.Helper()
	got, err := proc.Chain(r, hook.PID, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, w) {
		t.Fatalf("Chain = %v, want %v", got, w)
	}
}

func TestAlive(t *testing.T) {
	r := proctest.New()
	r.Line(hook, shell)
	if !proc.Alive(r, hook) {
		t.Fatal("running process not alive")
	}
	if proc.Alive(r, proc.ID{PID: hook.PID, Start: hook.Start + 1}) {
		t.Fatal("reused pid alive")
	}
	if proc.Alive(r, proc.ID{}) {
		t.Fatal("pid 0 alive")
	}
	r.Kill(hook.PID)
	if proc.Alive(r, hook) {
		t.Fatal("killed process alive")
	}
	r.Fail(shell.PID, errors.New("denied"))
	if proc.Alive(r, shell) {
		t.Fatal("unreadable process alive")
	}
}

func TestClientChain(t *testing.T) {
	r := proctest.New()
	me := proc.ID{PID: os.Getpid(), Start: 30}
	parent := proc.ID{PID: 4400, Start: 20}
	r.Line(me, parent, proc.ID{PID: 3010, Start: 10})
	if proc.ClientChain("linux", r) != nil || proc.ClientChain("darwin", r) != nil || proc.ClientChain("windows", nil) != nil {
		t.Fatal("chain outside Windows")
	}
	got := proc.ClientChain("windows", r)()
	if len(got) != 3 || got[0] != me || got[1] != parent {
		t.Fatalf("chain %v", got)
	}
}
