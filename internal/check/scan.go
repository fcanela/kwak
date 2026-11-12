// Package check holds kwak's message checks (spec 5.16): a built-in secrets scan and an optional
// check command. The scan is deterministic: regular expressions only, no model, no network. Its
// result names the kind of secret, never the secret itself.
package check

import (
	"regexp"
	"strings"
)

// pattern is one secret shape. A pattern whose value group (1 or more) is set gives the value to
// test with plausible, so placeholders such as "xxxx..." or "${TOKEN}" do not count. Its key group,
// when set, is the key name before the value; plausible needs it for 0x hex values.
type pattern struct {
	kind  string
	re    *regexp.Regexp
	value int // capture group of the value tested by plausible; 0 means none
	key   int // capture group of the key name; 0 means none (a Bearer value)
}

// patterns are tried in order, the narrow shapes before the general ones, so the kind is the most
// precise one.
var patterns = []pattern{
	{kind: "a private key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]{0,300}?[A-Za-z0-9+/]{40}`)},
	{kind: "a kwak owner key", re: regexp.MustCompile(`\bkwak_ok_[A-Za-z0-9_-]{32,}`)},
	{kind: "a GitHub token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{59})\b`)},
	{kind: "a GitLab token", re: regexp.MustCompile(`\bgl(?:pat|dt|rt|ptt|soat|cbt)-[A-Za-z0-9_-]{20,}`)},
	{kind: "a Slack token", re: regexp.MustCompile(`\b(?:xox[abposr]-[0-9A-Za-z-]{20,}|xapp-[0-9]-[A-Za-z0-9-]{20,})`)},
	{kind: "a Slack webhook URL", re: regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]{16,}`)},
	{kind: "an AWS access key id", re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{kind: "an Anthropic key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]*[A-Za-z0-9]{20}[A-Za-z0-9_-]*`)},
	{kind: "an OpenAI key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]*[A-Za-z0-9]{20}[A-Za-z0-9_-]*`)},
	{kind: "a JWT", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{16,}`)},
	{kind: "a Matrix access token", re: regexp.MustCompile(`\b(?:syt_[A-Za-z0-9]+_[A-Za-z0-9]{20}_[A-Za-z0-9]{6}|(?:mat|mct)_[A-Za-z0-9]{30,}(?:_[A-Za-z0-9]{6})?)\b`)},
	{kind: "a bearer token", re: regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/-]{20,}=*)`), value: 1},
	// The key name must end with the keyword (decision 38): GITHUB_TOKEN= and db_password: count,
	// tokenAddress= and token_id: do not. Matching is unanchored, so any prefix is fine.
	{kind: "a password, token or secret", re: regexp.MustCompile(
		`(?i)(password|passwd|passphrase|secret[_-]?key|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)["']?\s*[:=]\s*["']?([A-Za-z0-9_+/=.~!@#%^&*|?-]{16,})`),
		key: 1, value: 2},
}

// Scan reports the kind of the first secret shape found in text, for example "a GitHub token", or
// "" when there is none. The kind never holds any part of the text.
func Scan(text string) string {
	for _, p := range patterns {
		for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
			if p.value == 0 {
				return p.kind
			}
			key := ""
			if p.key > 0 {
				key = text[m[2*p.key]:m[2*p.key+1]]
			}
			if plausible(key, text[m[2*p.value]:m[2*p.value+1]]) {
				return p.kind
			}
		}
	}
	return ""
}

// hexValue matches a 0x hex value: 40 hex digits is an Ethereum address, 64 a private key or a
// hash.
var hexValue = regexp.MustCompile(`^0[xX]([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// plausible says a value next to a key name such as "token=", or after "Bearer" (key ""), looks
// like a real secret and not a placeholder, a word slug or an on-chain id (decision 38). A 0x value
// with 40 hex digits (an address) never counts; one with 64 counts only after a private key,
// secret, password or passphrase name, never after token, api key, access key or Bearer, where it
// is likely a hash or a token id. Any other value counts when it looks random: at least 16
// characters, letters and digits, at least 10 different characters, and not a word slug.
func plausible(key, v string) bool {
	v = strings.Trim(v, "=")
	if m := hexValue.FindStringSubmatch(v); m != nil {
		if len(m[1]) == 40 {
			return false
		}
		k := strings.ToLower(key)
		return strings.HasPrefix(k, "private") || strings.HasPrefix(k, "secret") || strings.HasPrefix(k, "pass")
	}
	if len(v) < 16 {
		return false
	}
	letter, digit := false, false
	seen := map[rune]bool{}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letter = true
		}
		seen[r] = true
	}
	return letter && digit && len(seen) >= 10 && !slug(v)
}

// slug says v is made of words and numbers, such as deploy-window-tuesday-14: 3 or more parts
// split on - _ and ., each only lowercase letters or only digits.
func slug(v string) bool {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts {
		lower, digits := true, true
		for _, r := range p {
			lower = lower && r >= 'a' && r <= 'z'
			digits = digits && r >= '0' && r <= '9'
		}
		if !lower && !digits {
			return false
		}
	}
	return true
}

// SecretReason is the reason given for a scan hit. It names the kind, never the secret.
func SecretReason(kind string) string {
	return "it looks like it holds a secret (" + kind + ")"
}
