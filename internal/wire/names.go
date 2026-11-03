package wire

import (
	"errors"
	"strings"
)

// Name errors.
var (
	ErrBadSessionID = errors.New("wire: session id is not a canonical UUID")
	ErrBadLocalpart = errors.New("wire: not an agent localpart")
	ErrBadHelper    = errors.New("wire: bad helper name")
)

const (
	agentPrefix    = "agent."
	standingSuffix = "listener"
)

// ValidSessionID accepts a canonical 8-4-4-4-12 hex UUID in any case and returns it lowercased.
func ValidSessionID(id string) (string, error) {
	if len(id) != 36 {
		return "", ErrBadSessionID
	}
	b := []byte(id)
	for i, c := range b {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", ErrBadSessionID
			}
			continue
		}
		switch {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
		case 'A' <= c && c <= 'F':
			b[i] = c + ('a' - 'A')
		default:
			return "", ErrBadSessionID
		}
	}
	return string(b), nil
}

// SessionHex returns the 32 lowercase hex digits of a valid session id.
func SessionHex(sessionID string) (string, error) {
	id, err := ValidSessionID(sessionID)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(id, "-", ""), nil
}

// Candidates returns the 5 session localparts in clash order (spec 3.6, DV-1): Claude takes a
// growing prefix of the hex, Codex a growing suffix. It returns nil for a bad owner, an unknown
// harness or hex32 that is not 32 lowercase hex digits.
func Candidates(owner string, h Harness, hex32 string) []string {
	if !OwnerName.MatchString(owner) || len(hex32) != 32 || !isHex(hex32) {
		return nil
	}
	out := make([]string, 0, 5)
	for n := 16; n <= 32; n += 4 {
		var part string
		switch h {
		case HarnessClaude:
			part = hex32[:n]
		case HarnessCodex:
			part = hex32[32-n:]
		default:
			return nil
		}
		out = append(out, agentPrefix+owner+"."+part)
	}
	return out
}

// HelperLocalpart returns the helper's localpart under the parent session localpart.
func HelperLocalpart(parent, helper string) (string, error) {
	n, err := ParseLocalpart(parent)
	if err != nil {
		return "", err
	}
	if n.Kind != KindSession {
		return "", ErrBadLocalpart
	}
	if !HelperName.MatchString(helper) {
		return "", ErrBadHelper
	}
	return parent + "." + helper, nil
}

// StandingLocalpart returns "agent.<owner>.listener".
func StandingLocalpart(owner string) string {
	return agentPrefix + owner + "." + standingSuffix
}

// ParseLocalpart parses an agent localpart of any of the three shapes of spec 3.6:
//
//	session   agent.<owner>.<hex 16, 20, 24, 28 or 32>
//	helper    agent.<owner>.<hex 16 to 32>.<helper>
//	standing  agent.<owner>.listener
func ParseLocalpart(lp string) (Name, error) {
	rest, ok := strings.CutPrefix(lp, agentPrefix)
	if !ok {
		return Name{}, ErrBadLocalpart
	}
	parts := strings.Split(rest, ".")
	if len(parts) < 2 || len(parts) > 3 || !OwnerName.MatchString(parts[0]) {
		return Name{}, ErrBadLocalpart
	}
	owner, id := parts[0], parts[1]
	if len(parts) == 3 {
		if len(id) < 16 || len(id) > 32 || !isHex(id) || !HelperName.MatchString(parts[2]) {
			return Name{}, ErrBadLocalpart
		}
		return Name{Kind: KindHelper, Owner: owner, Hex: id, Helper: parts[2]}, nil
	}
	if id == standingSuffix {
		return Name{Kind: KindStanding, Owner: owner}, nil
	}
	if len(id) < 16 || len(id) > 32 || len(id)%4 != 0 || !isHex(id) {
		return Name{}, ErrBadLocalpart
	}
	return Name{Kind: KindSession, Owner: owner, Hex: id}, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
