package wire

import (
	"slices"
	"testing"
)

func TestAPIError(t *testing.T) {
	e := &APIError{Status: 502, Errcode: ErrHomeserver, Message: "M_UNKNOWN"}
	if e.Error() != "KWAK_HOMESERVER: M_UNKNOWN" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestErrcodes(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Errcodes {
		if seen[c] {
			t.Errorf("%s listed twice", c)
		}
		seen[c] = true
		if StatusFor(c) < 400 {
			t.Errorf("StatusFor(%s) = %d", c, StatusFor(c))
		}
	}
	if StatusFor("KWAK_NOPE") != 0 || StatusFor("") != 0 {
		t.Error("unknown errcode has a status")
	}
	if !slices.Contains(Errcodes, ErrBadRequest) {
		t.Error("ErrBadRequest missing")
	}
}
