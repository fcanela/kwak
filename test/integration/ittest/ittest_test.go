//go:build integration

package ittest

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

func TestRunPattern(t *testing.T) {
	for name, want := range map[string]string{
		"TestSmoke":         "^TestSmoke$",
		"TestA/sub_case":    "^TestA$/^sub_case$",
		"TestB/x.y(1)/deep": `^TestB$/^x\.y\(1\)$/^deep$`,
	} {
		if got := runPattern(name); got != want {
			t.Errorf("runPattern(%q) = %q, want %q", name, got, want)
		}
	}
	if regexp.MustCompile(strings.Split(runPattern("TestSmoke"), "/")[0]).MatchString("TestSmokeTwo") {
		t.Error("pattern matches a longer name")
	}
}

func TestRenderConfig(t *testing.T) {
	p, err := templatePath()
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := renderConfig(string(text))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(conf), "\n")
	for _, want := range []string{
		`server_name = "kwak.test"`,
		`allow_federation = false`,
		`federate_created_rooms = false`,
		`allow_registration = true`,
		`registration_token_file = "/run/secrets/registration_token"`,
		`allow_guest_registration = false`,
		`grant_admin_to_first_user = true`,
		`ip_source = "rightmost_x_forwarded_for"`,
		`ip_source_trusted_subnets = []`,
		`error_on_unknown_config_opts = true`,
		`appservice_dir = "/run/secrets/appservices"`,
		`address = "0.0.0.0"`,
		`port = 8008`,
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	if err := guard("config", conf); err != nil {
		t.Error(err)
	}
	if _, err := renderConfig("{{.Nope}}"); err == nil {
		t.Error("unknown field rendered")
	}
}

func TestGuard(t *testing.T) {
	for _, bad := range []string{
		"kwak.labs." + forbidden + ".com",
		"x " + strings.ToUpper(forbidden) + " y",
	} {
		if guard("v", []byte(bad)) == nil {
			t.Errorf("guard passed %q", bad)
		}
		if guardArgs([]string{"run", "-e", "X=" + bad}) == nil {
			t.Errorf("guardArgs passed %q", bad)
		}
	}
	if err := guard("v", []byte("@agent.x:kwak.test")); err != nil {
		t.Error(err)
	}
	if _, err := docker(t.Context(), nil, "-e", forbidden); err == nil {
		t.Error("docker ran with a forbidden argument")
	}
}

func TestCheckSettings(t *testing.T) {
	set := func(kv ...string) []debug.BuildSetting {
		var s []debug.BuildSetting
		for i := 0; i < len(kv); i += 2 {
			s = append(s, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return s
	}
	if err := checkSettings("b", set("CGO_ENABLED", "0", "GOOS", "linux")); err != nil {
		t.Error(err)
	}
	for s, fix := range map[string]string{
		"CGO_ENABLED=1": "CGO_ENABLED=0 go test",
		"":              "CGO_ENABLED=0 go test",
		"darwin":        "needs linux",
	} {
		bs := set("CGO_ENABLED", "0", "GOOS", "darwin")
		switch s {
		case "CGO_ENABLED=1":
			bs = set("CGO_ENABLED", "1", "GOOS", "linux")
		case "":
			bs = set("GOOS", "linux")
		}
		err := checkSettings("b", bs)
		if err == nil || !strings.Contains(err.Error(), fix) {
			t.Errorf("%q: got %v, want an error naming %q", s, err, fix)
		}
	}
}

func TestWriteRootfs(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeRootfs(&buf, map[string]string{testBinary: src, "/bin/kwak": src}); err != nil {
		t.Fatal(err)
	}
	got := map[string]*tar.Header{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			if string(b) != "payload" {
				t.Errorf("%s holds %q", h.Name, b)
			}
		}
		got[h.Name] = h
	}
	for name, mode := range map[string]int64{
		"bin/": 0o755, "tmp/": 0o1777, "bin/kwak-it.test": 0o755, "bin/kwak": 0o755,
	} {
		h, ok := got[name]
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		if h.Mode != mode {
			t.Errorf("%s mode %o, want %o", name, h.Mode, mode)
		}
	}
	f := got[strings.TrimPrefix(ForeignDir, "/")+"/"]
	if f == nil || f.Typeflag != tar.TypeDir || f.Mode != 0o700 || f.Uid != ForeignUID || f.Gid != ForeignUID {
		t.Errorf("foreign dir header %+v", f)
	}
}

func TestEnvClientAllowsHTTP(t *testing.T) {
	var auth, xff string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, xff = r.Header.Get("Authorization"), r.Header.Get("X-Forwarded-For")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"displayname":"x"}`)
	}))
	defer srv.Close()
	e := &Env{Homeserver: srv.URL, HTTP: newHTTPClient(), Registration: wire.Registration{ASToken: "as"}}
	ok, err := e.AppserviceClient().ProfileExists(t.Context(), "@agent.x:kwak.test")
	if err != nil || !ok {
		t.Fatalf("ProfileExists over http:// = %v, %v", ok, err)
	}
	if auth != "Bearer as" || xff != ForwardedFor {
		t.Errorf("Authorization %q, X-Forwarded-For %q", auth, xff)
	}
	if c := e.Client("tok"); !c.AllowHTTP || c.Token != "tok" {
		t.Errorf("Client = %+v", c)
	}
}

func TestContainerArgs(t *testing.T) {
	tw := tuwunelArgs("n-tuwunel", "kwak-it-ab", "/h/tuwunel.toml", "/h/secrets", false)
	run := runnerArgs("n-runner", "kwak-it-ab", "", "/h/secrets", "kwak-it-runner:ab", "^TestX$", 90*time.Second)
	joined := func(a []string) string { return " " + strings.Join(a, " ") + " " }
	if tw[len(tw)-1] != TuwunelImage || strings.Contains(joined(tw), RunnerAlias) {
		t.Errorf("tuwunel args %q", tw)
	}
	for _, want := range []string{
		" --pull never ", " --network kwak-it-ab ", " --network-alias tuwunel ", " --read-only ",
		" --tmpfs /tmp ", " --mount type=tmpfs,destination=/var/lib/tuwunel,tmpfs-mode=1777 ",
		" --cap-drop ALL ", " --security-opt no-new-privileges:true ", " --user 65534:65534 ",
		" -e TUWUNEL_CONFIG=/etc/tuwunel/tuwunel.toml ",
		" --mount type=bind,source=/h/secrets,destination=/run/secrets,readonly ",
	} {
		if !strings.Contains(joined(tw), want) {
			t.Errorf("tuwunel args lack %q", want)
		}
	}
	for _, want := range []string{
		" --pull never ", " --network-alias kwak-it-runner ", " --read-only ", " --tmpfs /tmp ",
		" --cap-drop ALL ", " -e HOME=/tmp/home ", " -e KWAK_IT_INNER=1 ",
		" -test.run=^TestX$ ", " -test.timeout=1m30s ",
	} {
		if !strings.Contains(joined(run), want) {
			t.Errorf("runner args lack %q", want)
		}
	}
	for _, a := range [][]string{tw, run} {
		for _, bad := range []string{" --privileged ", " --network host ", " -p ", " --publish ", " -P "} {
			if strings.Contains(joined(a), bad) {
				t.Errorf("args contain %q", bad)
			}
		}
	}
	if strings.Contains(joined(runnerArgs("n", "w", "", "s", "t", "p", 0)), "-test.timeout") {
		t.Error("timeout set without a deadline")
	}

	tw = tuwunelArgs("n-tuwunel", "kwak-it-ab", "/h/tuwunel.toml", "/h/secrets", true)
	run = runnerArgs("n-runner", "kwak-it-ab", "n-tuwunel", "/h/secrets", "kwak-it-runner:ab", "^TestX$", 0)
	if !strings.Contains(joined(tw), " --network-alias tuwunel ") ||
		!strings.Contains(joined(tw), " --network-alias kwak-it-runner ") || tw[len(tw)-1] != TuwunelImage {
		t.Errorf("shared tuwunel args %q", tw)
	}
	if !strings.Contains(joined(run), " --network container:n-tuwunel ") || strings.Contains(joined(run), "--network-alias") {
		t.Errorf("shared runner args %q", run)
	}
}

func TestRegistration(t *testing.T) {
	raw, err := wire.GenRegistration(AppserviceURL, ServerName, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var r wire.Registration
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.URL != AppserviceURL {
		t.Errorf("url %q", r.URL)
	}
	if len(r.Namespaces.Users) != 1 || r.Namespaces.Users[0].Regex != `^@agent\..+:kwak\.test$` {
		t.Errorf("users namespace %+v", r.Namespaces.Users)
	}
	if err := guard("registration", raw); err != nil {
		t.Error(err)
	}
}

func TestCheckBinaryMatchesSelf(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if a, b := checkBinary(self), checkSelf(); (a == nil) != (b == nil) {
		t.Errorf("checkBinary %v, checkSelf %v", a, b)
	}
	if checkBinary(filepath.Join(t.TempDir(), "missing")) == nil {
		t.Error("missing binary accepted")
	}
}
