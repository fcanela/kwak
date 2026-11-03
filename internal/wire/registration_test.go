package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestGenRegistration(t *testing.T) {
	out, err := GenRegistration("http://kwak-it-runner:8009", "kwak.test", bytes.NewReader(seq(64)))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "id": "kwak",
  "url": "http://kwak-it-runner:8009",
  "as_token": "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
  "hs_token": "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f",
  "sender_localpart": "registrar",
  "rate_limited": false,
  "receive_ephemeral": false,
  "io.element.msc4190": false,
  "namespaces": {
    "users": [
      {
        "exclusive": true,
        "regex": "^@agent\\..+:kwak\\.test$"
      }
    ],
    "aliases": [],
    "rooms": []
  }
}
`
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	r, err := ParseRegistration(out)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(r.Namespaces.Users[0].Regex)
	for s, match := range map[string]bool{
		"@agent.fcanela.listener:kwak.test":       true,
		"@agent.x:kwak.test":                      true,
		"@fcanela:kwak.test":                      false,
		"@agentx.y:kwak.test":                     false,
		"@agent.x:kwakxtest":                      false,
		"@agent.x:kwak.test.evil":                 false,
		"@agent.x:kwak.test:8448":                 false,
		"@fcanela:kwak.test@agent.x:kwak.test":    false,
		"@fcanela.agent.x:kwak.test":              false,
		"x@agent.fcanela.listener:kwak.test":      false,
		"@agent.fcanela.listener:kwak.test\nfoo":  false,
		"@agent.fcanela.listener:other.kwak.test": false,
	} {
		if got := re.MatchString(s); got != match {
			t.Errorf("namespace regex on %q = %v, want %v", s, got, match)
		}
	}
}

func TestGenRegistrationFresh(t *testing.T) {
	a, err := GenRegistration("http://r:8009", "kwak.test", bytes.NewReader(seq(64)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenRegistration("http://r:8009", "kwak.test", bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	ra, _ := ParseRegistration(a)
	rb, _ := ParseRegistration(b)
	if ra.ASToken == rb.ASToken || ra.HSToken == rb.HSToken || ra.ASToken == ra.HSToken {
		t.Fatalf("tokens not fresh: %+v %+v", ra, rb)
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !hex64.MatchString(ra.ASToken) || !hex64.MatchString(ra.HSToken) {
		t.Fatalf("tokens are not 64 hex: %q %q", ra.ASToken, ra.HSToken)
	}
}

func TestGenRegistrationServerWithPort(t *testing.T) {
	out, err := GenRegistration("http://r:8009", "kwak.test:8448", bytes.NewReader(seq(64)))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ParseRegistration(out)
	if got := r.Namespaces.Users[0].Regex; got != `^@agent\..+:kwak\.test:8448$` {
		t.Fatalf("regex = %q", got)
	}
}

func TestGenRegistrationErrors(t *testing.T) {
	if _, err := GenRegistration("http://r:8009", "kwak.test", bytes.NewReader(seq(63))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short random: %v", err)
	}
	for _, url := range []string{"", "kwak-registrar:8009", "ftp://r", " http://r"} {
		if _, err := GenRegistration(url, "kwak.test", bytes.NewReader(seq(64))); err == nil {
			t.Errorf("url %q accepted", url)
		}
	}
	for _, name := range []string{"", "kwak test", `kwak".test`, "kwak.test\n", "kwak/test", "kwak\\.test", "kwak.test$", strings.Repeat("a", 256)} {
		if _, err := GenRegistration("http://r:8009", name, bytes.NewReader(seq(64))); err == nil {
			t.Errorf("server name %q accepted", name)
		}
	}
}

func TestParseRegistrationErrors(t *testing.T) {
	good, _ := GenRegistration("http://r:8009", "kwak.test", bytes.NewReader(seq(64)))
	edit := func(f func(m map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	cases := map[string][]byte{
		"not json":      []byte("id: kwak\n"),
		"unknown field": edit(func(m map[string]any) { m["extra"] = 1 }),
		"no as_token":   edit(func(m map[string]any) { delete(m, "as_token") }),
		"no hs_token":   edit(func(m map[string]any) { m["hs_token"] = "" }),
		"same tokens":   edit(func(m map[string]any) { m["hs_token"] = m["as_token"] }),
		"no sender":     edit(func(m map[string]any) { delete(m, "sender_localpart") }),
		"msc4190 on":    edit(func(m map[string]any) { m["io.element.msc4190"] = true }),
		"trailing":      append(append([]byte{}, good...), []byte("{}")...),
		"trailing }":    append(append([]byte{}, good...), '}'),
		"trailing ]":    append(append([]byte{}, good...), []byte(" ]")...),
	}
	for name, b := range cases {
		if _, err := ParseRegistration(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
