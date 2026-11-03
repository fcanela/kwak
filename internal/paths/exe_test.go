package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutable(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	file := func(p string) string {
		t.Helper()
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("kwak"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(p, to string) string {
		t.Helper()
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(to, p); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		return p
	}
	store := file("nix/store/abc-kwak/bin/kwak")
	other := file("go/bin/kwak")
	profile := link("profile/bin/kwak", store)
	selfLink := link("self/kwak", store)

	is := func(p string) func() (string, error) { return func() (string, error) { return p, nil } }
	on := func(p string) func(string) (string, error) {
		return func(name string) (string, error) {
			if name != "kwak" {
				t.Errorf("looked up %q", name)
			}
			return p, nil
		}
	}
	missing := func(string) (string, error) { return "", errors.New("not found") }

	cases := []struct {
		name string
		self func() (string, error)
		look func(string) (string, error)
		want string
	}{
		{"PATH links to this binary", is(store), on(profile), profile},
		{"PATH is another kwak", is(store), on(other), store},
		{"no kwak on PATH", is(store), missing, store},
		{"PATH entry is gone", is(store), on(filepath.Join(root, "gone/kwak")), store},
		{"running through a link", is(selfLink), missing, store},
		{"PATH is this binary itself", is(store), on(store), store},
	}
	for _, c := range cases {
		got, err := executable(c.self, c.look)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.name, got, err, c.want)
		}
	}
	if _, err := executable(func() (string, error) { return "", errors.New("no exe") }, missing); err == nil {
		t.Error("no error when the running binary is unknown")
	}
}
