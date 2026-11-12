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
	agentPrefix = "agent."
	// ListenerPart is the last part of a listener account's name (decision 42), and a helper name
	// no helper may take (ValidHelper).
	ListenerPart       = "listener"
	testStandingSuffix = "listener-test"
	testPairSuffix     = "test"
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
	if !ValidHelper(helper) {
		return "", ErrBadHelper
	}
	return parent + "." + helper, nil
}

// ValidHelper reports whether name may be a new helper's name: the HelperName shape, and not
// "listener", which listener account names end with (decision 42). ParseLocalpart still reads a
// helper named "listener" made before decision 42: its middle part is hex, so it is never taken
// for a listener account.
func ValidHelper(name string) bool {
	return name != ListenerPart && HelperName.MatchString(name)
}

// ListenerLocalpart returns the name of a pairing's listener account (decision 42):
// "agent.<owner>.<pair>.listener", or "agent.<owner>.<pair>-test.listener" for a test pairing.
// pair is a word pair, "<adjective>-<animal>" (ValidPair).
func ListenerLocalpart(owner, pair string, test bool) string {
	if test {
		pair += "-" + testPairSuffix
	}
	return agentPrefix + owner + "." + pair + "." + ListenerPart
}

// InAgentNamespace reports whether a localpart starts with "agent.", the namespace the registrar
// owns (spec 3.7). Every such account is an agent, so one that ParseLocalpart refuses is a mistake.
func InAgentNamespace(lp string) bool {
	return strings.HasPrefix(lp, agentPrefix)
}

// ParseLocalpart parses an agent localpart of any of the shapes of spec 3.6:
//
//	session        agent.<owner>.<hex 16, 20, 24, 28 or 32>
//	helper         agent.<owner>.<hex 16 to 32>.<helper>
//	listener       agent.<owner>.<word>-<word>.listener (Kind standing, Pair set; decision 42)
//	test listener  agent.<owner>.<word>-<word>-test.listener (Kind standing, Pair and Test set)
//	old standing   agent.<owner>.listener, agent.<owner>.listener-test (Kind standing, Pair empty)
//
// A three-part name is a listener account when its middle part holds a dash, and a helper when it
// is hex: hex never holds a dash, so the two never overlap. Each word is 1 to 16 lowercase letters
// and never "test"; the words are not checked against the lists, so a name stays readable if a
// list changes. The two old standing names are still read, so their records can be closed.
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
		if strings.Contains(id, "-") {
			pair, test := parsePairPart(id)
			if pair == "" || parts[2] != ListenerPart {
				return Name{}, ErrBadLocalpart
			}
			return Name{Kind: KindStanding, Owner: owner, Pair: pair, Test: test}, nil
		}
		if len(id) < 16 || len(id) > 32 || !isHex(id) || !HelperName.MatchString(parts[2]) {
			return Name{}, ErrBadLocalpart
		}
		return Name{Kind: KindHelper, Owner: owner, Hex: id, Helper: parts[2]}, nil
	}
	switch id {
	case ListenerPart:
		return Name{Kind: KindStanding, Owner: owner}, nil
	case testStandingSuffix:
		return Name{Kind: KindStanding, Owner: owner, Test: true}, nil
	}
	if len(id) < 16 || len(id) > 32 || len(id)%4 != 0 || !isHex(id) {
		return Name{}, ErrBadLocalpart
	}
	return Name{Kind: KindSession, Owner: owner, Hex: id}, nil
}

// parsePairPart reads the middle part of a listener account's name: "<word>-<word>", or
// "<word>-<word>-test". It returns "" when s is neither.
func parsePairPart(s string) (pair string, test bool) {
	w := strings.Split(s, "-")
	switch {
	case len(w) == 2:
	case len(w) == 3 && w[2] == testPairSuffix:
		test = true
	default:
		return "", false
	}
	if !validPairWord(w[0]) || !validPairWord(w[1]) {
		return "", false
	}
	return w[0] + "-" + w[1], test
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
