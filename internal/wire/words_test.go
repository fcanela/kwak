package wire

import (
	"bytes"
	"errors"
	"regexp"
	"testing"
)

// Each list has 64 distinct plain lowercase words, none of them "test" or "listener", so every
// pair makes a name ParseLocalpart reads back (decision 42).
func TestPairWordLists(t *testing.T) {
	plain := regexp.MustCompile(`^[a-z]{3,8}$`)
	seen := map[string]bool{}
	for _, list := range [][64]string{pairAdjectives, pairAnimals} {
		for _, w := range list {
			if !plain.MatchString(w) || w == "test" || w == ListenerPart || seen[w] {
				t.Errorf("word %q: not plain, reserved or repeated", w)
			}
			seen[w] = true
		}
	}
	if PairCount != 4096 {
		t.Fatalf("PairCount = %d", PairCount)
	}
	all := map[string]bool{}
	for i := range PairCount {
		p := PairAt(i)
		if !ValidPair(p) || all[p] {
			t.Fatalf("PairAt(%d) = %q: invalid or repeated", i, p)
		}
		all[p] = true
		for _, test := range []bool{false, true} {
			lp := ListenerLocalpart("fcanela", p, test)
			n, err := ParseLocalpart(lp)
			if err != nil || n.Kind != KindStanding || n.Pair != p || n.Test != test {
				t.Fatalf("ParseLocalpart(%q) = %+v, %v", lp, n, err)
			}
		}
	}
}

func TestRandomPair(t *testing.T) {
	p, err := RandomPair(bytes.NewReader([]byte{3, 23}))
	if err != nil || p != "brave-heron" {
		t.Fatalf("RandomPair = %q, %v", p, err)
	}
	// Each byte is taken modulo 64.
	if p, _ := RandomPair(bytes.NewReader([]byte{3 + 64, 23 + 192})); p != "brave-heron" {
		t.Fatalf("RandomPair with high bytes = %q", p)
	}
	if _, err := RandomPair(bytes.NewReader([]byte{1})); err == nil {
		t.Fatal("RandomPair with one byte left: no error")
	}
}

func TestFreePair(t *testing.T) {
	// The first random pick is taken, the second is free.
	used := map[string]bool{"brave-heron": true}
	p, err := FreePair(bytes.NewReader([]byte{3, 23, 6, 37}), func(s string) bool { return used[s] })
	if err != nil || p != "calm-otter" {
		t.Fatalf("FreePair = %q, %v", p, err)
	}
	// After 32 taken random picks it walks the lists: the first free pair in order.
	rnd := bytes.Repeat([]byte{3, 23}, 32)
	p, err = FreePair(bytes.NewReader(rnd), func(s string) bool { return s == "brave-heron" || s == PairAt(0) })
	if err != nil || p != PairAt(1) {
		t.Fatalf("FreePair after 32 clashes = %q, %v; want %q", p, err, PairAt(1))
	}
	if _, err := FreePair(bytes.NewReader(rnd), func(string) bool { return true }); !errors.Is(err, ErrNoPair) {
		t.Fatalf("FreePair with every pair taken: %v", err)
	}
	if _, err := FreePair(bytes.NewReader(nil), func(string) bool { return false }); err == nil {
		t.Fatal("FreePair with no random bytes: no error")
	}
}

func TestValidPair(t *testing.T) {
	for s, want := range map[string]bool{
		"brave-heron": true, "a-b": true,
		"brave": false, "brave-heron-test": false, "brave-test": false, "test-heron": false,
		"Brave-heron": false, "brave-heron-owl": false, "-heron": false, "brave-": false, "": false,
	} {
		if ValidPair(s) != want {
			t.Errorf("ValidPair(%q) = %v", s, !want)
		}
	}
}
