//go:build integration

// Package ittest runs a test against a real tuwunel on an internal docker network. On the host,
// Start builds a FROM scratch runner image holding the calling test binary, runs the calling test
// inside it next to tuwunel, streams its output, and returns nil. Inside the runner it returns the
// Env. Callers write:
//
//	env := ittest.Start(t)
//	if env == nil {
//		return
//	}
//
// Everything before Start runs twice, once on the host and once in the runner. The test binary
// must be built with CGO_ENABLED=0.
package ittest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/wire"
)

// Environment values the harness sets inside the runner.
const (
	EnvInner = "KWAK_IT_INNER" // "1" inside the runner

	// EnvSharedNetns, set to "1" on the host, runs the runner in tuwunel's network namespace, with
	// both names on tuwunel's endpoint. For hosts that drop traffic between containers on an
	// internal bridge. Bridge forwarding is then not exercised, which matters for fact probe F2.
	EnvSharedNetns = "KWAK_IT_SHARED_NETNS"
)

// Fixed names inside the docker network.
const (
	ServerName     = "kwak.test"
	HomeserverURL  = "http://tuwunel:8008"
	RunnerAlias    = "kwak-it-runner"
	AppserviceURL  = "http://kwak-it-runner:8009"
	AppserviceAddr = ":8009"
	TuwunelImage   = "ghcr.io/matrix-construct/tuwunel:v1.9.4"
	ForwardedFor   = "127.0.0.1"
)

// Paths inside both containers. The secrets directory is mounted read-only into the runner too,
// so tests read the registration tuwunel loaded.
const (
	SecretsDir       = "/run/secrets"
	RegistrationPath = SecretsDir + "/appservices/kwak.yaml"
	RegTokenPath     = SecretsDir + "/registration_token"
)

// A directory of another uid in the runner image, for tests of the owner checks (spec 10.2, ipc).
const (
	ForeignDir = "/run/kwak-it-foreign" // mode 0700
	ForeignUID = 4242
)

const (
	testBinary   = "/bin/kwak-it.test"
	runnerHome   = "/tmp/home"
	tuwunelConf  = "/etc/tuwunel/tuwunel.toml"
	label        = "kwak-it=1"
	readyTimeout = 90 * time.Second
)

// forbidden is the real server's domain label. Built by concatenation so the repo never spells it.
const forbidden = "acceso" + "prohibido"

// Option changes how Start builds the runner.
type Option func(*options)

type options struct {
	binaries map[string]string
}

// WithBinary ships the host file hostPath into the runner image as /bin/<name>. It must be a static
// CGO_ENABLED=0 build. Inside the runner hostPath is ignored.
func WithBinary(name, hostPath string) Option {
	return func(o *options) { o.binaries[name] = hostPath }
}

// Env is what a test sees inside the runner.
type Env struct {
	ServerName   string            // "kwak.test"
	Homeserver   string            // HomeserverURL
	RegToken     string            // registration token for humans
	Registration wire.Registration // as_token, hs_token and the rest, as tuwunel loaded it
	HTTP         *http.Client      // never follows redirects
	Binaries     map[string]string // name -> path inside the runner, from WithBinary

	// Registered by Start in this order, so Admin is the server admin.
	Admin, Alice, Bob matrix.Login
}

// Inner reports whether this process runs inside the runner.
func Inner() bool { return os.Getenv(EnvInner) == "1" }

// Start creates the network and containers on the host, or returns the Env inside the runner.
// t.Cleanup removes containers and the network, also on failure.
func Start(t *testing.T, opts ...Option) *Env {
	t.Helper()
	o := options{binaries: map[string]string{}}
	for _, opt := range opts {
		opt(&o)
	}
	if err := checkSelf(); err != nil {
		t.Fatal(err)
	}
	if Inner() {
		return startInner(t, o)
	}
	startHost(t, o)
	return nil
}

// Client returns a matrix.Client for token with ForwardedFor and AllowHTTP set.
func (e *Env) Client(token string) *matrix.Client {
	return &matrix.Client{Base: e.Homeserver, Token: token, ForwardedFor: ForwardedFor, AllowHTTP: true, HTTP: e.HTTP}
}

// AppserviceClient returns a matrix.Client with the as_token and ForwardedFor set.
func (e *Env) AppserviceClient() *matrix.Client { return e.Client(e.Registration.ASToken) }

// RegisterHuman registers localpart with RegToken and a random password, always with
// X-Forwarded-For. The first human registered becomes the server admin.
func (e *Env) RegisterHuman(t *testing.T, localpart string) matrix.Login {
	t.Helper()
	l, err := e.registerHuman(t.Context(), localpart, randHex(16))
	if err != nil {
		t.Fatalf("register %s: %v", localpart, err)
	}
	return l
}

// ServeAppservice serves h on AppserviceAddr until the test ends.
func (e *Env) ServeAppservice(t *testing.T, h http.Handler) {
	t.Helper()
	ln, err := net.Listen("tcp", AppserviceAddr)
	if err != nil {
		t.Fatalf("appservice listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
}

// ---- inside the runner ----

func startInner(t *testing.T, o options) *Env {
	t.Helper()
	if home := os.Getenv("HOME"); home != "" {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("make HOME: %v", err)
		}
	}
	raw, err := os.ReadFile(RegistrationPath)
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	e := &Env{
		ServerName: ServerName,
		Homeserver: HomeserverURL,
		HTTP:       newHTTPClient(),
		Binaries:   map[string]string{},
	}
	if err := json.Unmarshal(raw, &e.Registration); err != nil {
		t.Fatalf("parse registration: %v", err)
	}
	tok, err := os.ReadFile(RegTokenPath)
	if err != nil {
		t.Fatalf("read registration token: %v", err)
	}
	e.RegToken = strings.TrimSpace(string(tok))
	for name := range o.binaries {
		e.Binaries[name] = "/bin/" + name
	}
	if err := e.waitReady(t.Context(), readyTimeout); err != nil {
		t.Fatal(err)
	}
	e.Admin = e.RegisterHuman(t, "admin")
	e.Alice = e.RegisterHuman(t, "alice")
	e.Bob = e.RegisterHuman(t, "bob")
	return e
}

func newHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func (e *Env) waitReady(ctx context.Context, d time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var last error
	for {
		actx, acancel := context.WithTimeout(ctx, 3*time.Second)
		status, body, err := e.call(actx, http.MethodGet, "/_matrix/client/versions", "", nil)
		acancel()
		switch {
		case err == nil && status == http.StatusOK:
			return nil
		case err == nil:
			last = fmt.Errorf("status %d: %s", status, body)
		default:
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tuwunel not ready at %s after %s: %v; %s", e.Homeserver, d, last, netDiag())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// netDiag says whether the homeserver name resolves and takes TCP, to tell a host firewall from a
// slow start.
func netDiag() string {
	host := strings.TrimPrefix(HomeserverURL, "http://")
	name, _, _ := net.SplitHostPort(host)
	addrs, err := net.LookupHost(name)
	if err != nil {
		return fmt.Sprintf("lookup %s: %v", name, err)
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(addrs[0], "8008"), 3*time.Second)
	if err != nil {
		return fmt.Sprintf("%s resolves to %v but TCP fails (%v). If this host drops traffic between "+
			"containers on an internal bridge, rerun with %s=1", name, addrs, err, EnvSharedNetns)
	}
	c.Close()
	return fmt.Sprintf("%s resolves to %v and takes TCP", name, addrs)
}

func (e *Env) registerHuman(ctx context.Context, localpart, password string) (matrix.Login, error) {
	var l matrix.Login
	body := map[string]any{
		"username":                    localpart,
		"password":                    password,
		"initial_device_display_name": "kwak-it",
	}
	const path = "/_matrix/client/v3/register?kind=user"
	status, raw, err := e.call(ctx, http.MethodPost, path, "", body)
	if err != nil {
		return l, err
	}
	if status != http.StatusOK {
		if status != http.StatusUnauthorized {
			return l, fmt.Errorf("status %d: %s", status, raw)
		}
		var uia struct {
			Session string `json:"session"`
		}
		if err := json.Unmarshal(raw, &uia); err != nil {
			return l, fmt.Errorf("parse UIA reply: %v: %s", err, raw)
		}
		body["auth"] = map[string]any{
			"type":    "m.login.registration_token",
			"token":   e.RegToken,
			"session": uia.Session,
		}
		status, raw, err = e.call(ctx, http.MethodPost, path, "", body)
		if err != nil {
			return l, err
		}
		if status != http.StatusOK {
			return l, fmt.Errorf("status %d: %s", status, raw)
		}
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return l, fmt.Errorf("parse register reply: %v: %s", err, raw)
	}
	return l, nil
}

// call sends one request to the homeserver, always with X-Forwarded-For.
func (e *Env) call(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.Homeserver+path, r)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Forwarded-For", ForwardedFor)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// ---- on the host ----

func startHost(t *testing.T, o options) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find test binary: %v", err)
	}
	bins := map[string]string{testBinary: self}
	for name, p := range o.binaries {
		if name == "" || strings.ContainsAny(name, `/\`) || "/bin/"+name == testBinary {
			t.Fatalf("WithBinary: bad name %q", name)
		}
		if err := checkBinary(p); err != nil {
			t.Fatal(err)
		}
		bins["/bin/"+name] = p
	}

	tmplPath, err := templatePath()
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("read tuwunel template: %v", err)
	}
	conf, err := renderConfig(string(tmpl))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := wire.GenRegistration(AppserviceURL, ServerName, rand.Reader)
	if err != nil {
		t.Fatalf("generate registration: %v", err)
	}
	token := []byte(randHex(32))

	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	confPath := filepath.Join(dir, "tuwunel.toml")
	files := []struct {
		path string
		data []byte
	}{
		{confPath, conf},
		{filepath.Join(secrets, "registration_token"), token},
		{filepath.Join(secrets, "appservices", "kwak.yaml"), reg},
	}
	for _, f := range files {
		if err := guard(f.path, f.data); err != nil {
			t.Fatal(err)
		}
	}
	// tuwunel and the runner run as 65534, so the mounted tree is world readable. The test's
	// temp dir above it stays 0700 on the host.
	for _, d := range []string{secrets, filepath.Join(secrets, "appservices")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := randHex(4)
	network := "kwak-it-" + r
	tag := "kwak-it-runner:" + r
	tuwunelName := network + "-tuwunel"
	runnerName := network + "-runner"

	if _, err := docker(context.Background(), nil, "network", "create", "--internal", "--label", label, network); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanup(t, "network", "rm", network) })

	t.Cleanup(func() { cleanup(t, "image", "rm", "-f", tag) })
	if err := importImage(tag, bins); err != nil {
		t.Fatal(err)
	}

	shared := os.Getenv(EnvSharedNetns) == "1"
	if shared {
		t.Logf("ittest: %s=1, the runner shares tuwunel's network namespace", EnvSharedNetns)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := docker(context.Background(), nil, "logs", "--tail", "200", tuwunelName)
			t.Logf("tuwunel logs (last 200 lines):\n%s", out)
		}
		cleanup(t, "rm", "-f", "-v", tuwunelName)
	})
	if _, err := docker(context.Background(), nil, tuwunelArgs(tuwunelName, network, confPath, secrets, shared)...); err != nil {
		t.Fatal(err)
	}

	netns := ""
	if shared {
		netns = tuwunelName
	}
	args := runnerArgs(runnerName, network, netns, secrets, tag, runPattern(t.Name()), innerTimeout(t))
	if err := guardArgs(args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanup(t, "rm", "-f", "-v", runnerName) })
	ctx := context.Background()
	if dl, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, dl.Add(-5*time.Second))
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	out := t.Output()
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("runner exited with code %d", ee.ExitCode())
		}
		t.Fatalf("run runner: %v", err)
	}
}

func tuwunelArgs(name, network, confPath, secrets string, shared bool) []string {
	var alias []string
	if shared {
		alias = []string{"--network-alias", RunnerAlias}
	}
	return append(append([]string{
		"run", "-d", "--pull", "never",
		"--name", name, "--label", label,
		"--network", network, "--network-alias", "tuwunel",
		"--read-only", "--tmpfs", "/tmp",
		"--mount", "type=tmpfs,destination=/var/lib/tuwunel,tmpfs-mode=1777",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--user", "65534:65534",
		"--mount", "type=bind,source=" + confPath + ",destination=" + tuwunelConf + ",readonly",
		"--mount", "type=bind,source=" + secrets + ",destination=" + SecretsDir + ",readonly",
		"-e", "TUWUNEL_CONFIG=" + tuwunelConf,
	}, alias...), TuwunelImage)
}

// runnerArgs joins network with the RunnerAlias, or, with netns set, joins that container's
// network namespace (docker allows no alias there).
func runnerArgs(name, network, netns, secrets, tag, pattern string, timeout time.Duration) []string {
	nw := []string{"--network", network, "--network-alias", RunnerAlias}
	if netns != "" {
		nw = []string{"--network", "container:" + netns}
	}
	args := []string{"run", "--pull", "never", "--name", name, "--label", label}
	args = append(args, nw...)
	args = append(args,
		"--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--user", "65534:65534",
		"--mount", "type=bind,source="+secrets+",destination="+SecretsDir+",readonly",
		"-e", "HOME="+runnerHome,
		"-e", EnvInner+"=1",
		"-w", "/tmp",
		tag, testBinary,
		"-test.run="+pattern, "-test.v=true", "-test.count=1",
	)
	if timeout > 0 {
		args = append(args, "-test.timeout="+timeout.String())
	}
	return args
}

// runPattern anchors every level of a test name, so the runner runs exactly the calling test.
func runPattern(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	return strings.Join(parts, "/")
}

func innerTimeout(t *testing.T) time.Duration {
	dl, ok := t.Deadline()
	if !ok {
		return 0
	}
	left := time.Until(dl)
	if left > time.Minute {
		return (left - 30*time.Second).Round(time.Second)
	}
	return (left / 2).Round(time.Second)
}

type configData struct {
	ServerName            string
	RegistrationTokenFile string
	AppserviceDir         string
}

func renderConfig(text string) ([]byte, error) {
	tm, err := template.New("tuwunel.toml").Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse tuwunel template: %w", err)
	}
	var b bytes.Buffer
	err = tm.Execute(&b, configData{
		ServerName:            ServerName,
		RegistrationTokenFile: RegTokenPath,
		AppserviceDir:         filepath.Dir(RegistrationPath),
	})
	if err != nil {
		return nil, fmt.Errorf("render tuwunel template: %w", err)
	}
	return b.Bytes(), nil
}

// templatePath finds test/integration/testdata from this file's source path, so any calling
// package finds it. go test does not trim paths.
func templatePath() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("ittest: cannot locate its own source")
	}
	p := filepath.Join(filepath.Dir(file), "..", "testdata", "tuwunel.toml.tmpl")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("ittest: tuwunel template not found (built with -trimpath?): %w", err)
	}
	return p, nil
}

// importImage makes the FROM scratch runner image with docker import, which leaves no build cache.
func importImage(tag string, bins map[string]string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeRootfs(pw, bins)) }()
	_, err := docker(context.Background(), pr, "import", "--change", "LABEL "+label, "-", tag)
	pr.Close()
	return err
}

// writeRootfs writes a tar of an empty root holding bins (image path -> host path).
func writeRootfs(w io.Writer, bins map[string]string) error {
	tw := tar.NewWriter(w)
	now := time.Now()
	dirs := []struct {
		name string
		mode int64
		uid  int
	}{
		{"bin/", 0o755, 0}, {"etc/", 0o755, 0}, {"run/", 0o755, 0}, {"tmp/", 0o1777, 0},
		{strings.TrimPrefix(ForeignDir, "/") + "/", 0o700, ForeignUID},
	}
	for _, d := range dirs {
		h := &tar.Header{Typeflag: tar.TypeDir, Name: d.name, Mode: d.mode, Uid: d.uid, Gid: d.uid, ModTime: now}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(bins))
	for n := range bins {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := addFile(tw, strings.TrimPrefix(n, "/"), bins[n], now); err != nil {
			return err
		}
	}
	return tw.Close()
}

func addFile(tw *tar.Writer, name, src string, now time.Time) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o755, Size: st.Size(), ModTime: now}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func checkSelf() error {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return errors.New("ittest: no build info in the test binary")
	}
	return checkSettings("the test binary", bi.Settings)
}

func checkBinary(path string) error {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("ittest: read build info of %s: %w", path, err)
	}
	return checkSettings(path, bi.Settings)
}

// checkSettings wants a static linux build: a cgo DNS resolver cannot run in FROM scratch.
func checkSettings(what string, s []debug.BuildSetting) error {
	m := map[string]string{}
	for _, kv := range s {
		m[kv.Key] = kv.Value
	}
	if v := m["CGO_ENABLED"]; v != "0" {
		return fmt.Errorf("ittest: %s was built with CGO_ENABLED=%q, the FROM scratch runner needs 0. "+
			"Run CGO_ENABLED=0 go test -tags integration ..., or make integration. -race needs cgo, so drop it", what, v)
	}
	if v := m["GOOS"]; v != "linux" {
		return fmt.Errorf("ittest: %s was built for GOOS=%q, the runner needs linux", what, v)
	}
	return nil
}

func guard(what string, b []byte) error {
	if bytes.Contains(bytes.ToLower(b), []byte(forbidden)) {
		return fmt.Errorf("ittest: refusing to start: %s names the real server", what)
	}
	return nil
}

func guardArgs(args []string) error {
	for _, a := range args {
		if err := guard("a docker argument or environment value", []byte(a)); err != nil {
			return err
		}
	}
	return nil
}

func docker(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	if err := guardArgs(args); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %v: %s", args[0], err, bytes.TrimSpace(out))
	}
	return string(out), nil
}

func cleanup(t *testing.T, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := docker(ctx, nil, args...)
	if err != nil && !strings.Contains(strings.ToLower(out), "no such") {
		t.Errorf("cleanup: %v", err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
