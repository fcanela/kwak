package wire

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// The word lists of listener account names (decision 42): agent.<owner>.<adjective>-<animal>.listener.
// Each list has 64 plain, neutral, lowercase ASCII words, so one random byte picks a word with no
// bias (256 is a multiple of 64). No word is "test" or "listener", and no word holds a dash or a dot.
var (
	pairAdjectives = [64]string{
		"able", "amber", "bold", "brave", "bright", "brisk", "calm", "cheerful",
		"clear", "clever", "crisp", "curious", "eager", "early", "fair", "fast",
		"fine", "firm", "fresh", "friendly", "gentle", "glad", "golden", "good",
		"grand", "happy", "honest", "humble", "jolly", "keen", "kind", "lively",
		"loyal", "lucky", "merry", "mild", "modest", "neat", "nimble", "noble",
		"patient", "polite", "proud", "quick", "quiet", "rapid", "ready", "sharp",
		"shiny", "silver", "simple", "smart", "smooth", "snowy", "solid", "steady",
		"sturdy", "sunny", "swift", "tidy", "warm", "wise", "witty", "young",
	}
	pairAnimals = [64]string{
		"badger", "bear", "beaver", "bee", "bison", "camel", "cheetah", "crane",
		"deer", "dolphin", "duck", "eagle", "falcon", "ferret", "finch", "fox",
		"frog", "gecko", "giraffe", "goose", "hare", "hawk", "hedgehog", "heron",
		"horse", "ibis", "kiwi", "koala", "lark", "lemur", "lion", "llama",
		"lynx", "magpie", "marmot", "moose", "newt", "otter", "owl", "panda",
		"parrot", "pelican", "penguin", "puffin", "quail", "rabbit", "raven", "robin",
		"salmon", "seal", "sheep", "sparrow", "squirrel", "stork", "swan", "tiger",
		"toucan", "turtle", "walrus", "whale", "wolf", "wren", "yak", "zebra",
	}
)

// PairCount is how many word pairs exist: 64 adjectives times 64 animals.
const PairCount = len(pairAdjectives) * len(pairAnimals)

// ErrNoPair is returned when no word pair is free.
var ErrNoPair = errors.New("wire: no free word pair")

// PairAt returns word pair i, for i in [0, PairCount): "<adjective>-<animal>".
func PairAt(i int) string {
	return pairAdjectives[(i/len(pairAnimals))%len(pairAdjectives)] + "-" + pairAnimals[i%len(pairAnimals)]
}

// RandomPair reads two bytes from r and returns a word pair, "<adjective>-<animal>".
func RandomPair(r io.Reader) (string, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("wire: random: %w", err)
	}
	return pairAdjectives[int(b[0])%len(pairAdjectives)] + "-" + pairAnimals[int(b[1])%len(pairAnimals)], nil
}

// FreePair picks a word pair that used does not report as taken: up to 32 random picks from r,
// then the first free pair in list order, so a nearly full owner still gets a name. It returns
// ErrNoPair when all PairCount pairs are taken.
func FreePair(r io.Reader, used func(pair string) bool) (string, error) {
	for range 32 {
		p, err := RandomPair(r)
		if err != nil {
			return "", err
		}
		if !used(p) {
			return p, nil
		}
	}
	for i := range PairCount {
		if p := PairAt(i); !used(p) {
			return p, nil
		}
	}
	return "", ErrNoPair
}

// validPairWord is one word of a pair as ParseLocalpart reads it: 1 to 16 lowercase ASCII letters,
// never "test" (the suffix of a test pairing's name). It does not check the word lists, so a name
// stays readable if a list changes.
func validPairWord(w string) bool {
	if len(w) == 0 || len(w) > 16 || w == testPairSuffix {
		return false
	}
	for i := 0; i < len(w); i++ {
		if w[i] < 'a' || w[i] > 'z' {
			return false
		}
	}
	return true
}

// ValidPair reports whether s is a word pair, "<word>-<word>", with no test suffix.
func ValidPair(s string) bool {
	a, b, ok := strings.Cut(s, "-")
	return ok && validPairWord(a) && validPairWord(b)
}
