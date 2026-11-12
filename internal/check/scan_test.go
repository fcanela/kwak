package check

import (
	"strings"
	"testing"
)

// Fake secrets are built from pieces at test time, so no secret-shaped string sits in the repo
// for other scanners to find.

// mix is n letters and digits, mixed, the same each time.
func mix(n int) string {
	const abc = "aB3dE5gH7jK9mN2pQ4sT6vW8yZ0cF1hJ"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(abc[(i*7)%len(abc)])
	}
	return b.String()
}

func dash(parts ...string) string { return strings.Join(parts, "-") }

// armor is a PEM header or footer line, such as "BEGIN RSA PRIVATE KEY" between five dashes.
func armor(label string) string { return strings.Repeat("-", 5) + label + strings.Repeat("-", 5) }

type fixture struct {
	name, text, kind string
}

func positives() []fixture {
	pem := armor("BEGIN RSA PRIVATE KEY") + "\n" + mix(64) + "\n" + mix(40) + "\n" + armor("END RSA PRIVATE KEY")
	openssh := armor("BEGIN OPENSSH PRIVATE KEY") + "\n" + mix(70) + "\n"
	pgp := armor("BEGIN PGP PRIVATE KEY BLOCK") + "\n\n" + mix(64) + "\n"
	jwt := "ey" + "J" + mix(20) + ".ey" + "J" + mix(30) + "." + mix(43)
	return []fixture{
		{"rsa key block", "here it is:\n" + pem, "a private key"},
		{"openssh key block", openssh, "a private key"},
		{"pgp key block", pgp, "a private key"},
		{"kwak owner key", "use kwak" + "_ok_" + mix(43), "a kwak owner key"},
		{"github classic", "token gh" + "p_" + mix(36) + " works", "a GitHub token"},
		{"github app", "gh" + "s_" + mix(36), "a GitHub token"},
		{"github fine-grained", "github" + "_pat_" + mix(22) + "_" + mix(59), "a GitHub token"},
		{"gitlab", "export X=gl" + "pat-" + mix(20), "a GitLab token"},
		{"gitlab deploy", "gl" + "dt-" + mix(24), "a GitLab token"},
		{"slack bot", "xo" + "xb-" + dash("1234567890", "0987654321", mix(24)), "a Slack token"},
		{"slack app", "xa" + "pp-1-" + dash("A0B1C2D3E4", "1234567890", mix(30)), "a Slack token"},
		{"slack webhook", "https://hooks.slack." + "com/services/T0A1B2C3D/B0A1B2C3D/" + mix(24), "a Slack webhook URL"},
		{"aws key id", "AK" + "IA" + "Q3EGRAMDXV7YZ2KJ", "an AWS access key id"},
		{"aws session key id", "id AS" + "IA" + "Q3EGRAMDXV7YZ2KJ here", "an AWS access key id"},
		{"anthropic", "sk" + "-ant-api03-" + mix(80), "an Anthropic key"},
		{"openai project", "sk" + "-proj-" + mix(48) + "_" + mix(40), "an OpenAI key"},
		{"openai legacy", "OPENAI=sk" + "-" + mix(48), "an OpenAI key"},
		{"jwt", "cookie " + jwt, "a JWT"},
		{"matrix synapse", "sy" + "t_" + "YWdlbnQ_" + mix(20) + "_" + mix(6), "a Matrix access token"},
		{"matrix mas access", "ma" + "t_" + mix(32) + "_" + mix(6), "a Matrix access token"},
		{"matrix mas compat", "token mc" + "t_" + mix(40) + " here", "a Matrix access token"},
		{"bearer header", "Authorization: Bearer " + mix(32), "a bearer token"},
		{"password assignment", "DB_PASSWORD=" + "Tr0ub4dor" + "Horse" + "Battery9", "a password, token or secret"},
		{"token json", `{"access_token": "` + mix(32) + `"}`, "a password, token or secret"},
		{"secret yaml", "client_secret: " + mix(24), "a password, token or secret"},
		{"api key spaced", "api_key = '" + mix(20) + "'", "a password, token or secret"},
	}
}

func TestScanFindsEachShape(t *testing.T) {
	for _, f := range positives() {
		if got := Scan(f.text); got != f.kind {
			t.Errorf("%s: Scan = %q, want %q", f.name, got, f.kind)
		}
	}
}

func TestScanLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"How does the dispute kit pick jurors when the court has no stake?",
		"the token is on the other machine; ask the owner for it",
		"password: see the vault",
		"token=${GITHUB_TOKEN}",
		"api_key = xxxxxxxxxxxxxxxxxxxxxxxx",
		"secret: " + strings.Repeat("ab", 12),
		"set token = self.request.headers.authorization",
		"Bearer tokens go in the Authorization header, never in a URL.",
		"Bearer " + strings.Repeat("x", 30),
		armor("BEGIN PRIVATE KEY") + " starts a PEM block; never paste one.",
		"thread $Fz8xTq3kLmN0pQrStUvWxYz0123456789abcdefgh:kwak.example.org",
		"@agent.fcanela.4f2a9c1e7b034d5a:kwak.example.org",
		"session 6b29fc40-ca47-4a2b-9c3d-1f5e8a7b6c0d",
		"sha256 " + strings.Repeat("9f86d081884c7d65", 4),
		"commit 7a01730c4b2f8e9d1a3b5c7d9e0f1a2b3c4d5e6f",
		"https://github.com/fcanela/kwak/pull/12#issuecomment-123456789",
		"ghp_ is the prefix of a classic GitHub token",
		"sk" + "-this-is-a-long-kebab-case-name-for-a-thing",
		"the task-runner-with-a-long-kebab-case-name works",
		"ey" + "J" + mix(20) + " is only one part of a JWT",
		"AKIA alone, or AK" + "IA" + "SHORT",
		"xo" + "xb- is a Slack prefix",
		"glpat is the GitLab prefix",
		"sy" + "t_ tokens come from Synapse",
		"ma" + "t_ and mc" + "t_ tokens come from MAS",
		"ma" + "t_" + mix(12) + " is too short",
		"format_" + mix(40),
		"github" + "_pat_ starts a fine-grained token",
		"github" + "_pat_" + mix(22) + " is only one part",
		"sk" + "-ant-" + "this-is-a-long-kebab-case-name-for-a-thing",
		"https://hooks.slack." + "com/services/T0A1B2C3D/B0A1B2C3D/" + mix(8),
		"https://hooks.slack." + "com/services/ is the webhook prefix",
		"gl" + "dt- is a GitLab deploy token prefix",
		armor("BEGIN PGP PRIVATE KEY BLOCK") + " starts an exported key.",
		"kwak" + "_ok_ starts an owner key",
		"base64 " + mix(80),
	} {
		if got := Scan(s); got != "" {
			t.Errorf("Scan(%q) = %q, want nothing", s, got)
		}
	}
}

// The keyword rule of decision 38: the key name must end with the keyword, the value must look
// random, and a 0x hex value counts only as a private key, secret or password, never as an address
// or after token or api key. Each row is one example from the decision's brief.
func TestScanKeywordRule(t *testing.T) {
	const kv = "a password, token or secret"
	hex64 := "0x" + strings.Repeat("9f86d081884c7d65", 4)
	addr := "0x6B175474E89094C44Da98b954EedeAC495271d0F" // a public contract address
	for _, c := range []struct{ name, text, kind string }{
		{"api key", "api_key=" + "Zx81kQ0pLm" + "2vB7tR9sWc", kv},
		{"quoted db password", `DB_PASSWORD="` + "h7Gk2pQx" + "9LmZ4vB1" + `"`, kv},
		{"private key in hex", "private_key: " + hex64, kv},
		{"secret in hex", "secret=" + hex64, kv},
		{"passphrase in hex", "PASSPHRASE: " + hex64, kv},
		{"access token", "access_token=" + mix(32), kv},
		{"prefixed key name", "GITHUB_TOKEN=" + mix(24), kv},
		{"prefixed password", "db_password: " + mix(20), kv},
		{"secret key", "SECRET_KEY = '" + mix(40) + "'", kv},
		{"token is an address", "token: " + addr, ""},
		{"token address", "tokenAddress=" + addr, ""},
		{"private key is an address", "private_key: " + addr, ""},
		{"token id", "token_id: 12345678901234567890", ""},
		{"secret slug", "secret: deploy-window-tuesday-14", ""},
		{"passphrase of words", "password: correcthorsebatterystaple", ""},
		{"token hash", "token: " + hex64, ""},
		{"api key hash", "api_key=" + hex64, ""},
		{"access key hash", "access_key=" + hex64, ""},
		{"secret name", "secretName=" + mix(24), ""},
		{"token suffix", "tokenValue: " + mix(24), ""},
		{"few different characters", "token=" + strings.Repeat("ab12", 6), ""},
		{"bearer hash", "Authorization: Bearer " + hex64, ""},
		{"bearer slug", "Bearer deploy-window-tuesday-and-wednesday-14", ""},
		{"bearer few characters", "Bearer " + strings.Repeat("ab12", 6), ""},
		{"bearer random", "Bearer " + mix(24), "a bearer token"},
	} {
		if got := Scan(c.text); got != c.kind {
			t.Errorf("%s: Scan(%q) = %q, want %q", c.name, c.text, got, c.kind)
		}
	}
}

func TestSlug(t *testing.T) {
	for v, want := range map[string]bool{
		"deploy-window-tuesday-14": true,
		"release_2026.11.12":       true,
		"a-b":                      false,
		"deploy-Window-tuesday":    false,
		"x9-window-tuesday":        false,
		mix(24):                    false,
	} {
		if got := slug(v); got != want {
			t.Errorf("slug(%q) = %v, want %v", v, got, want)
		}
	}
}

// The reason for a hit names the kind only: no piece of the secret, so a refusal or a notice
// never repeats it (spec 5.16).
func TestSecretReasonNeverEchoes(t *testing.T) {
	for _, f := range positives() {
		reason := SecretReason(Scan(f.text))
		for _, w := range strings.FieldsFunc(f.text, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
		}) {
			if len(w) >= 12 && strings.Contains(reason, w) {
				t.Errorf("%s: reason %q repeats %q", f.name, reason, w)
			}
		}
		if strings.Contains(reason, mix(8)) {
			t.Errorf("%s: reason %q repeats the secret", f.name, reason)
		}
	}
}
