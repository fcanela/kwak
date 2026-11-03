package osproc

import (
	"testing"

	"github.com/fcanela/kwak/internal/proc"
)

func stat(comm, rest string) []byte {
	return []byte("4242 (" + comm + ") " + rest + "\n")
}

// rest of a stat line after the command name: state, ppid, then fields 5 to 22 (start = 987654).
const tail = " 77 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 200"

func TestParseStat(t *testing.T) {
	cases := []struct {
		name, comm, state string
		zombie            bool
	}{
		{"plain", "bash", "S", false},
		{"spaces", "my prog name", "R", false},
		{"paren", "a) b (c", "S", false},
		{"fake fields", ") S 1 2 3", "S", false},
		{"zombie", "sh", "Z", true},
		{"dead", "sh", "X", true},
		{"empty", "", "S", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ppid, start, zombie, err := parseStat(stat(c.comm, c.state+tail))
			if err != nil {
				t.Fatal(err)
			}
			if ppid != 77 || start != 987654 || zombie != c.zombie {
				t.Fatalf("got ppid %d start %d zombie %v", ppid, start, zombie)
			}
		})
	}
	bad := map[string][]byte{
		"no paren":   []byte("4242 bash S 1 2"),
		"short":      stat("bash", "S 77 1 2 3"),
		"bad ppid":   stat("bash", "S x"+tail[3:]),
		"bad start":  []byte("4242 (bash) S 77 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 -5 1000"),
		"empty file": nil,
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseStat(b); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestOlderParent(t *testing.T) {
	cases := []struct {
		name               string
		child, parentStart uint64
		ppid               int
		want               proc.ID
	}{
		{"older parent", 200, 100, 9, proc.ID{PID: 9, Start: 100}},
		{"younger parent is a reused pid", 200, 300, 9, proc.ID{}},
		{"same time", 200, 200, 9, proc.ID{}},
		{"parent time unknown", 200, 0, 9, proc.ID{}},
		{"child time unknown", 0, 100, 9, proc.ID{}},
		{"no parent", 200, 100, 0, proc.ID{}},
	}
	for _, c := range cases {
		if got := olderParent(c.child, c.ppid, c.parentStart); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
