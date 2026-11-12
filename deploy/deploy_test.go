// Package deploy checks the deployment files in deploy/registrar and docs/deploy.md.
package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/wire"
)

const regDir = "registrar"

// scanned returns every file the secret and style checks cover.
func scanned(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join("..", "docs", "deploy.md"))
	return files
}

var (
	hexRun     = regexp.MustCompile(`[0-9a-fA-F]{32,}`)
	ownerKey   = regexp.MustCompile(`kwak_ok_[A-Za-z0-9_-]{16,}`)
	privateKey = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	padded     = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={1,2}`)
	bearer     = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9+/_=.-]{16,}`)
	longWord   = regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}`)
	// A secret-named key with a literal value. Placeholders (<...>), templates ({{ }}) and shell
	// variables ($x) are allowed, and so is an empty value.
	secretKV = regexp.MustCompile(`(?i)(?:^|[^<\w])(as_token|hs_token|access_token|owner_key|password|passwd|registration_token)["']?\s*[:=]\s*["']?([^\s"'<>{}$,;)` + "`" + `]+)`)
)

// finding names what kind of secret matched and how long it is, never its text.
type finding struct {
	kind string
	n    int
}

func (f finding) String() string { return fmt.Sprintf("%s (%d chars)", f.kind, f.n) }

// secretsIn returns the parts of text that look like a real token, key or password.
func secretsIn(text string) []finding {
	var found []finding
	add := func(kind string, ms []string) {
		for _, m := range ms {
			found = append(found, finding{kind, len(m)})
		}
	}
	add("hex", hexRun.FindAllString(text, -1))
	add("owner key", ownerKey.FindAllString(text, -1))
	add("private key", privateKey.FindAllString(text, -1))
	add("padded base64", padded.FindAllString(text, -1))
	add("bearer token", bearer.FindAllString(text, -1))
	for _, w := range longWord.FindAllString(text, -1) {
		if randomLooking(w) {
			found = append(found, finding{"random-looking word", len(w)})
		}
	}
	for _, m := range secretKV.FindAllStringSubmatch(text, -1) {
		found = append(found, finding{m[1] + " value", len(m[2])})
	}
	return found
}

// randomLooking is true for a word with a mixed-case run that has a digit or scores at least 6,
// as random tokens do and names, paths and identifiers here do not. The score counts upper case
// letters not followed by lower case, plus lone lower case letters: CamelCase has few of both.
// Measured on 2M random 32-byte keys: about 3 in a million base64url keys and 5 in 100,000
// 32-character alphanumeric tokens are missed. The worst CamelCase test names in this repo score 5.
func randomLooking(w string) bool {
	score := 0
	for _, run := range strings.FieldsFunc(w, func(r rune) bool { return strings.ContainsRune("+/_-", r) }) {
		var up, low, dig bool
		for _, r := range run {
			up = up || unicode.IsUpper(r)
			low = low || unicode.IsLower(r)
			dig = dig || unicode.IsDigit(r)
		}
		if !up || !low {
			continue
		}
		if dig {
			return true
		}
		lower := func(i int) bool { return i >= 0 && i < len(run) && unicode.IsLower(rune(run[i])) }
		for i := range len(run) {
			if unicode.IsUpper(rune(run[i])) && !lower(i+1) {
				score++
			}
			if lower(i) && !lower(i-1) && !lower(i+1) {
				score++
			}
		}
	}
	return score >= 6
}

// vector returns n fixed bytes derived from seed, so the scanner tests never depend on luck.
func vector(seed string, n int) []byte {
	var out []byte
	for i := 0; len(out) < n; i++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "%s %d", seed, i))
		out = append(out, sum[:]...)
	}
	return out[:n]
}

func letters(b []byte, alphabet string) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return sb.String()
}

const (
	alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	alnum = alpha + "0123456789"
)

func TestScannerFindsRealShapes(t *testing.T) {
	b := vector("kwak deploy scanner", 32)
	url := strings.NewReplacer("-", "", "_", "").Replace(base64.RawURLEncoding.EncodeToString(b))
	hex24 := hex.EncodeToString(b[:24])
	cases := map[string]string{
		"as_token":         `"as_token": "` + hex.EncodeToString(b) + `"`,
		"owner key":        "kwak_ok_" + base64.RawURLEncoding.EncodeToString(b),
		"matrix token":     "Authorization: Bearer " + url,
		"wireguard":        "PrivateKey = " + base64.StdEncoding.EncodeToString(b),
		"pem":              "-----BEGIN OPENSSH PRIVATE KEY-----",
		"password":         `"password": "hunter2"`,
		"yaml token":       "registration_token: abc123",
		"no digit":         "tok " + letters(vector("kwak deploy no digit", 44), alpha),
		"short hex":        "tok=" + hex24,
		"short hex bearer": "Authorization: Bearer " + hex24,
		"bearer":           "Authorization: Bearer abcdefghijklmnopqr",
	}
	for name, text := range cases {
		if len(secretsIn(text)) == 0 {
			t.Errorf("%s: not found in %q", name, text)
		}
	}
	clean := []string{
		`"as_token": "<as_token: 64 hex, random, written by gen-registration>"`,
		"egress_deny_interfaces: [eth1, wg0]",
		`proxy_pass http://192.0.2.12:8010;`,
		`if ($args ~* "(^|&)access_token=") { return 400; }`,
		"the refusal of `?access_token=` stays",
		`password: {{ lookup }}`,
		`as_token = $AS`,
		`headers={"Authorization": "Bearer " + reg["as_token"],`,
		"deploy/registrar/Dockerfile.dockerignore",
		"$STACK_DIR/data/kwak-registrar-backup/",
		"TestDMLeaveInvitesBackThenOpensANewDM",
		"TestHomeserverAdapterAllowsHTTPOnlyForItsOwnBase",
		"TestEveryErrcodeInHTTPTable",
	}
	for _, text := range clean {
		if got := secretsIn(text); len(got) > 0 {
			t.Errorf("false positive in %q: %v", text, got)
		}
	}
}

// TestScannerFindsRandomKeys feeds many fixed random-looking keys in the shapes real tokens take.
func TestScannerFindsRandomKeys(t *testing.T) {
	const n = 5000
	shapes := map[string]func([]byte) string{
		"base64": base64.StdEncoding.EncodeToString,
		"base64url": func(b []byte) string {
			return strings.NewReplacer("-", "", "_", "").Replace(base64.RawURLEncoding.EncodeToString(b))
		},
		"alnum":  func(b []byte) string { return letters(b, alnum) },
		"hex 24": func(b []byte) string { return hex.EncodeToString(b[:24]) },
		"hex 16": func(b []byte) string { return hex.EncodeToString(b[:16]) },
	}
	for name, shape := range shapes {
		missed := 0
		for i := range n {
			if len(secretsIn(shape(vector(fmt.Sprint("kwak deploy ", name, i), 32)))) == 0 {
				missed++
			}
		}
		if missed > 0 {
			t.Errorf("%s: %d of %d keys missed", name, missed, n)
		}
	}
}

func TestFindingNeverHoldsTheSecret(t *testing.T) {
	secret := hex.EncodeToString(vector("kwak deploy finding", 32))
	found := secretsIn(`"as_token": "` + secret + `"`)
	if len(found) == 0 {
		t.Fatal("not found")
	}
	out := fmt.Sprint(found)
	for i := 0; i+8 <= len(secret); i++ {
		if strings.Contains(out, secret[i:i+8]) {
			t.Fatalf("the report %q holds part of the secret", out)
		}
	}
}

func TestNoSecretsInDeployFiles(t *testing.T) {
	for _, p := range scanned(t) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if p == "deploy_test.go" {
			continue // holds only the scanner's patterns; TestScannerFindsRealShapes covers it
		}
		for _, f := range secretsIn(string(data)) {
			t.Errorf("%s: looks like a real secret: %v", p, f)
		}
	}
}

func TestNoEmDash(t *testing.T) {
	for _, p := range scanned(t) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.ContainsRune(data, rune(0x2014)) {
			t.Errorf("%s contains an em-dash", p)
		}
	}
}

// loadConfig runs config.example.json through the registrar's own loader. The loader also reads
// the registration the config names, so that path points at a fresh one in a temp dir.
func loadConfig(t *testing.T) registrar.Config {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(regDir, "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if raw["registration"] != "/run/secrets/appservices/kwak.yaml" {
		t.Errorf("registration = %v, want it under the ./secrets mount", raw["registration"])
	}
	server, _ := raw["server_name"].(string)
	reg, err := wire.GenRegistration("http://kwak-registrar:8009", server, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw["registration"] = filepath.Join(dir, "kwak.yaml")
	if err := os.WriteFile(raw["registration"].(string), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	conf, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(confPath, conf, 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := registrar.LoadConfig(confPath)
	if err != nil {
		t.Fatalf("config.example.json does not load: %v", err)
	}
	return c
}

func TestConfigExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(regDir, "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"idle_after", "close_grace", "key_lifetime", "key_warn_before",
		"pairing_ttl", "unconfirmed_ttl", "prune_closed_after", "jobs_every",
		"report_room", "test_room", "test_report_room"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("config.example.json does not show %s", k)
		}
	}
	c := loadConfig(t)
	if c.Homeserver != "http://tuwunel:8008" {
		t.Errorf("homeserver = %q, want the docker-network URL http://tuwunel:8008 (D1)", c.Homeserver)
	}
	if path.Dir(c.State) != "/var/lib/kwak-registrar" {
		t.Errorf("state = %q, want it in the ./data mount", c.State)
	}
	if port(t, c.AppserviceAddr) != "8009" || port(t, c.APIAddr) != "8010" {
		t.Errorf("ports = %s and %s, want 8009 and 8010", c.AppserviceAddr, c.APIAddr)
	}
	if len(c.Admins) == 0 {
		t.Error("no admins")
	}
	// The side rooms of decisions 39 and 40: optional, and the -test suffix names the test ones.
	if c.ReportRoom == "" || !strings.HasPrefix(c.TestRoom, "#agents-test:") || !strings.HasPrefix(c.TestReportRoom, "#agents-report-test:") {
		t.Errorf("side rooms %q %q %q", c.ReportRoom, c.TestRoom, c.TestReportRoom)
	}
}

func port(t *testing.T, addr string) string {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%q: %v", addr, err)
	}
	return p
}

func TestRegistrationExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(regDir, "registration.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := wire.ParseRegistration(data)
	if err != nil {
		t.Fatalf("registration.example.json: %v", err)
	}
	c := loadConfig(t)
	gen, err := wire.GenRegistration(got.URL, c.ServerName, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	want, err := wire.ParseRegistration(gen)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ASToken, "<") || !strings.HasPrefix(got.HSToken, "<") {
		t.Error("the example tokens must be placeholders")
	}
	got.ASToken, got.HSToken = want.ASToken, want.HSToken
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if !bytes.Equal(g, w) {
		t.Errorf("example differs from gen-registration's output:\n got %s\nwant %s", g, w)
	}
	if got.URL != "http://kwak-registrar:8009" {
		t.Errorf("url = %q, want the compose alias on port 8009", got.URL)
	}
}

// checkGenerated checks a registration printed by gen-registration, without logging its tokens.
func checkGenerated(out []byte, url, server string) error {
	r, err := wire.ParseRegistration(out)
	if err != nil {
		return fmt.Errorf("does not parse: %w", err)
	}
	if len(r.Namespaces.Users) != 1 {
		return fmt.Errorf("%d users namespaces, want exactly one", len(r.Namespaces.Users))
	}
	if r.URL != url || r.Namespaces.Users[0].Regex != wire.UserNamespace(server) {
		return fmt.Errorf("url %q, users namespace %q", r.URL, r.Namespaces.Users[0].Regex)
	}
	return nil
}

func TestCheckGenerated(t *testing.T) {
	const url, server = "http://kwak-registrar:8009", "kwak.test"
	good, err := wire.GenRegistration(url, server, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGenerated(good, url, server); err != nil {
		t.Errorf("gen-registration's own output: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(good, &m); err != nil {
		t.Fatal(err)
	}
	m["namespaces"].(map[string]any)["users"] = []any{}
	empty, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGenerated(empty, url, server); err == nil {
		t.Error("a registration with no users namespace passed")
	}
	if err := checkGenerated(good, url, "other.test"); err == nil {
		t.Error("a registration for another server passed")
	}
}

func readText(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(regDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNginxLocations(t *testing.T) {
	conf := readText(t, "nginx-locations.example.conf")
	for _, want := range []string{
		"location = /_kwak/v1/pair {",
		"location ^~ /_kwak/v1/ {",
		"limit_req zone=matrix_auth",
		"proxy_set_header X-Forwarded-For $remote_addr;",
		"proxy_pass http://192.0.2.12:8010;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("nginx-locations.example.conf lacks %q", want)
		}
	}
	if n := strings.Count(conf, "limit_req zone="); n != 1 {
		t.Errorf("%d rate limits, want only the one on /pair (DV-11)", n)
	}
	if strings.Contains(conf, "/_matrix/app") || strings.Contains(conf, "8009") {
		t.Error("the appservice side must never be routed")
	}
	if strings.Contains(conf, "proxy_pass http://192.0.2.12:8010/") {
		t.Error("proxy_pass must have no URI part")
	}
}

func TestNftablesEntry(t *testing.T) {
	y := readText(t, "nftables.example.yml")
	for _, want := range []string{
		"- name: kwak-registrar-from-proxy", "protocol: tcp", "port: 8010", "- 192.0.2.4/32",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("nftables.example.yml lacks %q", want)
		}
	}
}

func TestDockerfile(t *testing.T) {
	d := readText(t, "Dockerfile")
	from := regexp.MustCompile(`(?m)^FROM golang:(\S+) AS build$`).FindStringSubmatch(d)
	if from == nil || !regexp.MustCompile(`^1\.26\.\d+-`).MatchString(from[1]) {
		t.Errorf("the build stage must use an exact golang 1.26.x tag, got %v", from)
	}
	for _, want := range []string{"CGO_ENABLED=0", "-trimpath", "./cmd/kwak-registrar", "FROM scratch",
		"USER 65532:65532", `ENTRYPOINT ["/kwak-registrar"]`} {
		if !strings.Contains(d, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	ign := readText(t, "Dockerfile.dockerignore")
	for _, want := range []string{"!cmd/kwak-registrar", "!internal", "!go.mod", "!go.sum"} {
		if !strings.Contains(ign, want) {
			t.Errorf("Dockerfile.dockerignore lacks %q", want)
		}
	}
}

// composeConfig is the part of `docker compose config --format json` the checks read.
type composeConfig struct {
	Services map[string]struct {
		Image       string   `json:"image"`
		User        string   `json:"user"`
		ReadOnly    bool     `json:"read_only"`
		CapDrop     []string `json:"cap_drop"`
		SecurityOpt []string `json:"security_opt"`
		PidsLimit   int      `json:"pids_limit"`
		Command     []string `json:"command"`
		NetworkMode string   `json:"network_mode"`
		Networks    map[string]struct {
			Aliases []string `json:"aliases"`
		} `json:"networks"`
		Ports []struct {
			HostIP    string `json:"host_ip"`
			Target    int    `json:"target"`
			Published string `json:"published"`
		} `json:"ports"`
		Volumes []struct {
			Target   string `json:"target"`
			ReadOnly bool   `json:"read_only"`
		} `json:"volumes"`
	} `json:"services"`
	Networks map[string]struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	} `json:"networks"`
}

func compose(t *testing.T, file string) composeConfig {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", file, "config", "--format", "json")
	cmd.Dir = regDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config %s: %v\n%s", file, err, stderr.String())
	}
	var c composeConfig
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func checkHardened(t *testing.T, c composeConfig) {
	t.Helper()
	s, ok := c.Services["kwak-registrar"]
	if !ok {
		t.Fatal("no kwak-registrar service")
	}
	if s.User == "" || strings.HasPrefix(s.User, "0:") || !s.ReadOnly || !slices.Equal(s.CapDrop, []string{"ALL"}) ||
		!slices.Contains(s.SecurityOpt, "no-new-privileges:true") || s.PidsLimit <= 0 {
		t.Errorf("service is not locked down: %+v", s)
	}
	if !slices.Equal(s.Command, []string{"serve", "-config", "/etc/kwak-registrar/config.json"}) {
		t.Errorf("command = %v", s.Command)
	}
	mounts := map[string]bool{}
	for _, v := range s.Volumes {
		mounts[v.Target] = v.ReadOnly
	}
	want := map[string]bool{"/etc/kwak-registrar/config.json": true, "/run/secrets": true, "/var/lib/kwak-registrar": false}
	if len(mounts) != len(want) {
		t.Errorf("mounts = %v, want %v", mounts, want)
	}
	for target, ro := range want {
		if got, ok := mounts[target]; !ok || got != ro {
			t.Errorf("mount %s: present %v, read-only %v; want read-only %v", target, ok, got, ro)
		}
	}
}

func TestComposeExample(t *testing.T) {
	c := compose(t, "compose.example.yaml")
	checkHardened(t, c)
	s := c.Services["kwak-registrar"]
	if !slices.Contains(s.Networks["tuwunel"].Aliases, "kwak-registrar") {
		t.Errorf("no kwak-registrar alias on the tuwunel network: %+v", s.Networks)
	}
	if n := c.Networks["tuwunel"]; !n.External || n.Name != "tuwunel_default" {
		t.Errorf("tuwunel network = %+v, want the external tuwunel_default", n)
	}
	if len(s.Ports) == 0 {
		t.Fatal("no published ports")
	}
	for _, p := range s.Ports {
		if p.Target != 8010 || p.Published != "8010" || p.HostIP == "" || p.HostIP == "0.0.0.0" {
			t.Errorf("published %+v: only 8010, on a named address, may be published", p)
		}
	}
}

func TestComposeNetnsFallback(t *testing.T) {
	c := compose(t, "compose.netns-fallback.example.yaml")
	checkHardened(t, c)
	s := c.Services["kwak-registrar"]
	if !strings.HasPrefix(s.NetworkMode, "container:") || len(s.Ports) > 0 {
		t.Errorf("fallback: network_mode %q, ports %v", s.NetworkMode, s.Ports)
	}
}

func readDoc(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "docs", "deploy.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestDocNeverPrintsTheRegistration checks that the generator output is always piped, never shown.
func TestDocNeverPrintsTheRegistration(t *testing.T) {
	doc := readDoc(t)
	code := codeBlocks(doc)
	gens := strings.Count(code, "gen-registration")
	if gens == 0 {
		t.Fatal("no gen-registration in deploy.md")
	}
	if n := len(regexp.MustCompile(`-o - \\\n\s*\| `).FindAllString(code, -1)); n != gens {
		t.Errorf("%d of %d gen-registration commands pipe their output with -o -", n, gens)
	}
}

// codeBlocks returns the lines inside the fenced code blocks of a markdown text.
func codeBlocks(md string) string {
	var sb strings.Builder
	in := false
	for line := range strings.Lines(md) {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			continue
		}
		if in {
			sb.WriteString(line)
		}
	}
	return sb.String()
}
