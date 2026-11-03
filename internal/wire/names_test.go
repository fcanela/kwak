package wire

import (
	"errors"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

func TestValidSessionID(t *testing.T) {
	good := []struct{ in, want string }{
		{"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d", "4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d"},
		{"01a198dd-ad17-79f0-8261-713321e78fea", "01a198dd-ad17-79f0-8261-713321e78fea"},
		{"4F2A9C1E-7B03-4D5A-8C0D-1E2F3A4B5C6D", "4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d"},
		{"01A198dd-Ad17-79F0-8261-713321E78FEA", "01a198dd-ad17-79f0-8261-713321e78fea"},
		{"00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000"},
	}
	for _, c := range good {
		got, err := ValidSessionID(c.in)
		if err != nil || got != c.want {
			t.Errorf("ValidSessionID(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	bad := []string{
		"",
		"-4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6",
		"-f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6-",
		"4f2a9c1e/7b03-4d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b/c6d",
		"../a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5..d",
		"4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d",
		"{4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d}",
		"urn:uuid:4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d ",
		" 4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d\n",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d0",
		"4f2a9c1-e7b03-4d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b034-d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a8-c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a-8c0d1-e2f3a4b5c6d",
		"4f2a9c1e_7b03_4d5a_8c0d_1e2f3a4b5c6d",
		"4f2a9c1g-7b03-4d5a-8c0d-1e2f3a4b5c6d",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6z",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c٠",
		"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5é",
		"zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz",
		"------------------------------------",
	}
	for _, in := range bad {
		if got, err := ValidSessionID(in); !errors.Is(err, ErrBadSessionID) || got != "" {
			t.Errorf("ValidSessionID(%q) = %q, %v; want ErrBadSessionID", in, got, err)
		}
		if got, err := SessionHex(in); !errors.Is(err, ErrBadSessionID) || got != "" {
			t.Errorf("SessionHex(%q) = %q, %v; want ErrBadSessionID", in, got, err)
		}
	}
}

func TestSessionHex(t *testing.T) {
	cases := []struct{ in, want string }{
		{"4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d", "4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d"},
		{"01A198DD-AD17-79F0-8261-713321E78FEA", "01a198ddad1779f08261713321e78fea"},
	}
	for _, c := range cases {
		got, err := SessionHex(c.in)
		if err != nil || got != c.want {
			t.Errorf("SessionHex(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestCandidates(t *testing.T) {
	v4, _ := SessionHex("4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d")
	v7, _ := SessionHex("01a198dd-ad17-79f0-8261-713321e78fea")
	cases := []struct {
		owner string
		h     Harness
		hex   string
		want  []string
	}{
		{"fcanela", HarnessClaude, v4, []string{
			"agent.fcanela.4f2a9c1e7b034d5a",
			"agent.fcanela.4f2a9c1e7b034d5a8c0d",
			"agent.fcanela.4f2a9c1e7b034d5a8c0d1e2f",
			"agent.fcanela.4f2a9c1e7b034d5a8c0d1e2f3a4b",
			"agent.fcanela.4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d",
		}},
		{"mallory", HarnessCodex, v7, []string{
			"agent.mallory.8261713321e78fea",
			"agent.mallory.79f08261713321e78fea",
			"agent.mallory.ad1779f08261713321e78fea",
			"agent.mallory.98ddad1779f08261713321e78fea",
			"agent.mallory.01a198ddad1779f08261713321e78fea",
		}},
		{"b0b_x-1", HarnessClaude, v7, []string{
			"agent.b0b_x-1.01a198ddad1779f0",
			"agent.b0b_x-1.01a198ddad1779f08261",
			"agent.b0b_x-1.01a198ddad1779f082617133",
			"agent.b0b_x-1.01a198ddad1779f08261713321e7",
			"agent.b0b_x-1.01a198ddad1779f08261713321e78fea",
		}},
	}
	for _, c := range cases {
		got := Candidates(c.owner, c.h, c.hex)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("Candidates(%q, %q, %q) =\n%q\nwant\n%q", c.owner, c.h, c.hex, got, c.want)
		}
		last := ""
		for i, lp := range got {
			n, err := ParseLocalpart(lp)
			if err != nil || n.Kind != KindSession || n.Owner != c.owner || len(n.Hex) != 16+4*i {
				t.Errorf("ParseLocalpart(%q) = %+v, %v", lp, n, err)
			}
			if i > 0 && !strings.Contains(n.Hex, last) {
				t.Errorf("step %d %q does not extend %q", i, n.Hex, last)
			}
			last = n.Hex
		}
	}
	bad := []struct {
		owner string
		h     Harness
		hex   string
	}{
		{"fcanela", "gemini", v4},
		{"fcanela", "", v4},
		{"", HarnessClaude, v4},
		{"f.canela", HarnessClaude, v4},
		{"Fcanela", HarnessClaude, v4},
		{"fcanela", HarnessClaude, v4[:31]},
		{"fcanela", HarnessClaude, v4 + "0"},
		{"fcanela", HarnessClaude, strings.ToUpper(v4)},
		{"fcanela", HarnessCodex, "4f2a9c1e-7b03-4d5a-8c0d-1e2f3a4b5c6d"},
		{"fcanela", HarnessCodex, "g" + v4[1:]},
	}
	for _, c := range bad {
		if got := Candidates(c.owner, c.h, c.hex); got != nil {
			t.Errorf("Candidates(%q, %q, %q) = %q; want nil", c.owner, c.h, c.hex, got)
		}
	}
}

func TestStandingLocalpart(t *testing.T) {
	lp := StandingLocalpart("fcanela")
	if lp != "agent.fcanela.listener" {
		t.Fatalf("StandingLocalpart = %q", lp)
	}
	n, err := ParseLocalpart(lp)
	if err != nil || n != (Name{Kind: KindStanding, Owner: "fcanela"}) {
		t.Fatalf("ParseLocalpart(%q) = %+v, %v", lp, n, err)
	}
}

func TestHelperLocalpart(t *testing.T) {
	good := []struct{ parent, helper, want string }{
		{"agent.fcanela.4f2a9c1e7b034d5a", "tests", "agent.fcanela.4f2a9c1e7b034d5a.tests"},
		{"agent.fcanela.4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d", "a", "agent.fcanela.4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d.a"},
		{"agent.fcanela.8261713321e78fea", "x-1-" + strings.Repeat("y", 20), "agent.fcanela.8261713321e78fea.x-1-" + strings.Repeat("y", 20)},
		{"agent.fcanela.8261713321e78fea", "listener", "agent.fcanela.8261713321e78fea.listener"},
	}
	for _, c := range good {
		got, err := HelperLocalpart(c.parent, c.helper)
		if err != nil || got != c.want {
			t.Errorf("HelperLocalpart(%q, %q) = %q, %v; want %q", c.parent, c.helper, got, err, c.want)
			continue
		}
		n, err := ParseLocalpart(got)
		want := Name{Kind: KindHelper, Owner: "fcanela", Hex: strings.Split(c.parent, ".")[2], Helper: c.helper}
		if err != nil || n != want {
			t.Errorf("ParseLocalpart(%q) = %+v, %v; want %+v", got, n, err, want)
		}
	}
	bad := []struct {
		parent, helper string
		err            error
	}{
		{"agent.fcanela.listener", "tests", ErrBadLocalpart},
		{"agent.fcanela.4f2a9c1e7b034d5a.tests", "more", ErrBadLocalpart},
		{"agent.fcanela.4f2a9c1e7b034", "tests", ErrBadLocalpart},
		{"fcanela.4f2a9c1e7b034d5a", "tests", ErrBadLocalpart},
		{"agent.fcanela.4f2a9c1e7b034d5a", "", ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "Tests", ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "1tests", ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "-tests", ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "te_sts", ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "te.sts", ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "te/sts", ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "a" + strings.Repeat("b", 24), ErrBadHelper},
		{"agent.fcanela.4f2a9c1e7b034d5a", "tests\n", ErrBadHelper},
	}
	for _, c := range bad {
		if got, err := HelperLocalpart(c.parent, c.helper); !errors.Is(err, c.err) || got != "" {
			t.Errorf("HelperLocalpart(%q, %q) = %q, %v; want %v", c.parent, c.helper, got, err, c.err)
		}
	}
}

func TestParseLocalpart(t *testing.T) {
	hex32 := "4f2a9c1e7b034d5a8c0d1e2f3a4b5c6d"
	owner32 := "a" + strings.Repeat("b", 31)
	good := []struct {
		in   string
		want Name
	}{
		{"agent.fcanela.4f2a9c1e7b034d5a", Name{KindSession, "fcanela", "4f2a9c1e7b034d5a", ""}},
		{"agent.fcanela." + hex32, Name{KindSession, "fcanela", hex32, ""}},
		{"agent.0.4f2a9c1e7b034d5a", Name{KindSession, "0", "4f2a9c1e7b034d5a", ""}},
		{"agent." + owner32 + ".listener", Name{KindStanding, owner32, "", ""}},
		{"agent.x-_.listener", Name{KindStanding, "x-_", "", ""}},
		{"agent.fcanela.4f2a9c1e7b034d5a.tests", Name{KindHelper, "fcanela", "4f2a9c1e7b034d5a", "tests"}},
		{"agent.fcanela.4f2a9c1e7b034d5a8.tests", Name{KindHelper, "fcanela", "4f2a9c1e7b034d5a8", "tests"}},
		{"agent.fcanela." + hex32 + ".t", Name{KindHelper, "fcanela", hex32, "t"}},
	}
	for _, c := range good {
		got, err := ParseLocalpart(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseLocalpart(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	bad := []string{
		"",
		"agent",
		"agent.",
		"agent.fcanela",
		"agent.fcanela.",
		"agent..4f2a9c1e7b034d5a",
		"agents.fcanela.4f2a9c1e7b034d5a",
		"Agent.fcanela.4f2a9c1e7b034d5a",
		"xagent.fcanela.4f2a9c1e7b034d5a",
		"@agent.fcanela.4f2a9c1e7b034d5a:kwak.test",
		"agent.fcanela.4f2a9c1e7b034d5a:kwak.test",
		"agent.Fcanela.4f2a9c1e7b034d5a",
		"agent._fcanela.4f2a9c1e7b034d5a",
		"agent.-fcanela.4f2a9c1e7b034d5a",
		"agent." + owner32 + "c.listener",
		"agent.fcanela.4F2A9C1E7B034D5A",
		"agent.fcanela.4f2a9c1e7b034d5",
		"agent.fcanela.4f2a9c1e7b034d5a8",
		"agent.fcanela.4f2a9c1e7b034d5a8c",
		"agent.fcanela.4f2a9c1e7b034d5a8c0",
		"agent.fcanela." + hex32 + "0000",
		"agent.fcanela.4f2a9c1e7b034d5g",
		"agent.fcanela.Listener",
		"agent.fcanela.listener.tests",
		"agent.fcanela.listeners",
		"agent.fcanela.4f2a9c1e7b034d5.tests",
		"agent.fcanela." + hex32 + "0.tests",
		"agent.fcanela.4f2a9c1e7b034d5a.",
		"agent.fcanela.4f2a9c1e7b034d5a.Tests",
		"agent.fcanela.4f2a9c1e7b034d5a.a" + strings.Repeat("b", 24),
		"agent.fcanela.4f2a9c1e7b034d5a.tests.more",
		"agent.fcanela.4f2a9c1e7b034d5a.tests.",
		"agent.fcanela.4f2a9c1e7b034d5a\n",
		"agent.fcanela.4f2a9c1e7b034d5a ",
	}
	for _, in := range bad {
		if got, err := ParseLocalpart(in); !errors.Is(err, ErrBadLocalpart) || got != (Name{}) {
			t.Errorf("ParseLocalpart(%q) = %+v, %v; want ErrBadLocalpart", in, got, err)
		}
	}
}

func TestNameRegexes(t *testing.T) {
	owners := map[string]bool{
		"fcanela": true, "0": true, "a" + strings.Repeat("-", 31): true, "a_b": true,
		"": false, "f.canela": false, "Fcanela": false, "_a": false, "-a": false,
		"a" + strings.Repeat("b", 32): false, "a b": false, "fcanela\n": false,
	}
	for s, want := range owners {
		if got := OwnerName.MatchString(s); got != want {
			t.Errorf("OwnerName(%q) = %v, want %v", s, got, want)
		}
	}
	helpers := map[string]bool{
		"tests": true, "a": true, "a" + strings.Repeat("9", 23): true, "a-b": true,
		"": false, "1a": false, "-a": false, "a_b": false, "A": false, "a.b": false,
		"a" + strings.Repeat("9", 24): false, "tests\n": false,
	}
	for s, want := range helpers {
		if got := HelperName.MatchString(s); got != want {
			t.Errorf("HelperName(%q) = %v, want %v", s, got, want)
		}
	}
}

// The three shapes of spec 3.6, written out with the owner as a group.
var (
	shapeSession  = regexp.MustCompile(`^agent\.([a-z0-9][a-z0-9_-]{0,31})\.[0-9a-f]{16}([0-9a-f]{4}){0,4}$`)
	shapeHelper   = regexp.MustCompile(`^agent\.([a-z0-9][a-z0-9_-]{0,31})\.[0-9a-f]{16,32}\.[a-z][a-z0-9-]{0,23}$`)
	shapeStanding = regexp.MustCompile(`^agent\.([a-z0-9][a-z0-9_-]{0,31})\.listener$`)
)

func shapeOf(lp string) (Kind, int) {
	var k Kind
	n := 0
	if shapeSession.MatchString(lp) {
		k, n = KindSession, n+1
	}
	if shapeHelper.MatchString(lp) {
		k, n = KindHelper, n+1
	}
	if shapeStanding.MatchString(lp) {
		k, n = KindStanding, n+1
	}
	return k, n
}

func checkShapes(t *testing.T, lp string) Kind {
	t.Helper()
	k, n := shapeOf(lp)
	if n > 1 {
		t.Fatalf("%q matches %d shapes", lp, n)
	}
	got, err := ParseLocalpart(lp)
	if n == 0 {
		if err == nil {
			t.Fatalf("ParseLocalpart(%q) = %+v; matches no shape", lp, got)
		}
		return ""
	}
	if err != nil || got.Kind != k {
		t.Fatalf("ParseLocalpart(%q) = %+v, %v; shape is %s", lp, got, err, k)
	}
	return k
}

func TestShapesNeverOverlap(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	seen := map[Kind]int{}
	atoms := []string{"agent", "agent.", ".", "listener", "fcanela", "a", "-", "_", "0", "f", "g",
		"4f2a", "9c1e7b03", "4d5a8c0d1e2f3a4b", "tests", "x"}
	alphabet := "abcdefghijklmnopqrstuvwxyz0123456789._-"
	for i := 0; i < 200000; i++ {
		var b strings.Builder
		if r.IntN(4) > 0 {
			b.WriteString("agent.")
		}
		for j, n := 0, r.IntN(12); j < n; j++ {
			if r.IntN(3) == 0 {
				b.WriteByte(alphabet[r.IntN(len(alphabet))])
			} else {
				b.WriteString(atoms[r.IntN(len(atoms))])
			}
		}
		seen[checkShapes(t, b.String())]++
	}
	hex := "0123456789abcdef"
	for i := 0; i < 50000; i++ {
		var b strings.Builder
		b.WriteString("agent.o.")
		for j, n := 0, 14+r.IntN(21); j < n; j++ {
			b.WriteByte(hex[r.IntN(16)])
		}
		switch r.IntN(3) {
		case 0:
			b.WriteString(".h")
		case 1:
			b.WriteString(".listener")
		}
		seen[checkShapes(t, b.String())]++
	}
	for _, k := range []Kind{KindSession, KindHelper, KindStanding, ""} {
		if seen[k] < 100 {
			t.Errorf("only %d random strings of kind %q; the generator is too weak", seen[k], k)
		}
	}
	t.Logf("kinds seen: %v", seen)
}

func FuzzParseLocalpart(f *testing.F) {
	for _, s := range []string{"agent.fcanela.listener", "agent.fcanela.4f2a9c1e7b034d5a",
		"agent.fcanela.4f2a9c1e7b034d5a.tests", "agent.a.b.c.d", "agent..", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, lp string) {
		_ = checkShapes(t, lp)
		n, err := ParseLocalpart(lp)
		if err != nil {
			return
		}
		var back string
		switch n.Kind {
		case KindStanding:
			back = StandingLocalpart(n.Owner)
		case KindSession:
			back = "agent." + n.Owner + "." + n.Hex
		case KindHelper:
			back = "agent." + n.Owner + "." + n.Hex + "." + n.Helper
		}
		if back != lp {
			t.Fatalf("ParseLocalpart(%q) = %+v, which rebuilds as %q", lp, n, back)
		}
	})
}
