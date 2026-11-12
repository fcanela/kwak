// Command kwak-registrar is the kwak appservice on the Matrix host: serve, gen-registration, create-room.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/registrar"
	"github.com/fcanela/kwak/internal/wire"
)

// deps are the OS pieces the commands use, so tests can replace them.
type deps struct {
	ctx        context.Context
	isTerminal func(any) bool
	http       matrix.Doer // create-room, to the public homeserver URL
	hsHTTP     matrix.Doer // serve, to tuwunel
	ready      func(api, appservice net.Addr)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := deps{ctx: ctx, isTerminal: isTerminal, http: matrix.NewHTTPClient(), hsHTTP: matrix.NewHTTPClient()}
	code := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, d)
	stop()
	os.Exit(code)
}

const usage = `usage:
  kwak-registrar serve -config <config.json>
  kwak-registrar gen-registration -url <appservice url> -server <server name> [-format json|yaml] -o <path|->
  kwak-registrar create-room -homeserver <https url> [-server <server name>] [-alias <name>] [-invite-only]
                             (access token on stdin; -alias defaults to agents)
`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, d deps) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stderr, d)
	case "gen-registration":
		return genRegistration(args[1:], stdout, stderr, d)
	case "create-room":
		return createRoom(args[1:], stdin, stdout, stderr, d)
	case "version":
		fmt.Fprintf(stdout, "kwak-registrar %s\n", wire.Version)
		return 0
	}
	fmt.Fprintf(stderr, "kwak-registrar: unknown command %q\n%s", args[0], usage)
	return 2
}

// isTerminal reports whether f is a character device, such as a terminal.
func isTerminal(f any) bool {
	file, ok := f.(*os.File)
	if !ok {
		return false
	}
	fi, err := file.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func serve(args []string, stderr io.Writer, d deps) int {
	fs := flags("serve", stderr)
	path := fs.String("config", "/etc/kwak-registrar/config.json", "config file")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	h := slog.NewTextHandler(stderr, nil)
	log := slog.New(h)
	cfg, asToken, err := registrar.LoadConfig(*path)
	if err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
		return 1
	}
	store, err := registrar.OpenStore(cfg.State)
	if err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: %v. It never resets its state: restore state.json from a backup.\n", err)
		return 1
	}
	srv := &registrar.Server{
		Cfg: cfg, Store: store, HS: registrar.NewHomeserver(cfg.Homeserver, asToken, d.hsHTTP),
		Clock: registrar.SystemClock{}, Rand: rand.Reader, Log: log,
	}
	errLog := slog.NewLogLogger(h, slog.LevelWarn)
	asLn, err := net.Listen("tcp", cfg.AppserviceAddr)
	if err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
		return 1
	}
	apiLn, err := net.Listen("tcp", cfg.APIAddr)
	if err != nil {
		asLn.Close()
		fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
		return 1
	}
	servers := []*http.Server{
		{Handler: srv.Appservice(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 40 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: errLog},
		{Handler: srv.API(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 40 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: errLog},
	}
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()
	failed := make(chan error, 2)
	for i, ln := range []net.Listener{asLn, apiLn} {
		go func() {
			if err := servers[i].Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		}()
	}
	var wg sync.WaitGroup
	wg.Go(func() { srv.RunJobs(ctx) })
	log.Info("kwak-registrar started", "appservice", asLn.Addr().String(), "api", apiLn.Addr().String(), "version", wire.Version)
	if d.ready != nil {
		d.ready(apiLn.Addr(), asLn.Addr())
	}
	code := 0
	select {
	case <-ctx.Done():
	case err := <-failed:
		log.Error("server failed", "err", err)
		code = 1
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	cancel()
	wg.Wait()
	if err := store.Flush(); err != nil {
		log.Error("flush", "err", err)
		code = 1
	}
	log.Info("kwak-registrar stopped")
	return code
}

var yamlTmpl = template.Must(template.New("kwak.yaml").Funcs(template.FuncMap{"q": quote}).Parse(
	`id: {{q .ID}}
url: {{q .URL}}
as_token: {{q .ASToken}}
hs_token: {{q .HSToken}}
sender_localpart: {{q .SenderLocalpart}}
rate_limited: false
receive_ephemeral: false
io.element.msc4190: false
namespaces:
  users:
{{- range .Namespaces.Users}}
    - exclusive: {{.Exclusive}}
      regex: {{q .Regex}}
{{- end}}
  aliases: []
  rooms: []
`))

// quote writes a YAML double-quoted scalar; JSON string escapes are valid there.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func genRegistration(args []string, stdout, stderr io.Writer, d deps) int {
	fs := flags("gen-registration", stderr)
	u := fs.String("url", "", "appservice URL tuwunel calls, for example http://kwak-registrar:8009")
	server := fs.String("server", "", "server name")
	format := fs.String("format", "json", "json (read by tuwunel and the registrar) or yaml")
	out := fs.String("o", "", "output file, created with mode 0600; - for stdout (never a terminal)")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	if *u == "" || *server == "" || *out == "" {
		fmt.Fprintln(stderr, "kwak-registrar: gen-registration needs -url, -server and -o")
		return 2
	}
	if *format != "json" && *format != "yaml" {
		fmt.Fprintln(stderr, "kwak-registrar: -format is json or yaml")
		return 2
	}
	data, err := wire.GenRegistration(*u, *server, rand.Reader)
	if err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
		return 1
	}
	if *format == "yaml" {
		var r wire.Registration
		if err := json.Unmarshal(data, &r); err != nil {
			fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
			return 1
		}
		var b bytes.Buffer
		if err := yamlTmpl.Execute(&b, r); err != nil {
			fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
			return 1
		}
		data = b.Bytes()
	}
	if *out == "-" {
		if d.isTerminal(stdout) {
			fmt.Fprintln(stderr, "kwak-registrar: refusing to print the registration to a terminal; pipe it, for example into your secret store")
			return 1
		}
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
			return 1
		}
		return 0
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
		return 1
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(*out)
		fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "Wrote %s (mode 0600).\n", *out)
	return 0
}

func createRoom(args []string, stdin io.Reader, stdout, stderr io.Writer, d deps) int {
	fs := flags("create-room", stderr)
	hsURL := fs.String("homeserver", "", "homeserver URL, https://")
	server := fs.String("server", "", "server name (default: the homeserver's host name)")
	alias := fs.String("alias", "agents", "room alias name, for example agents-report or agents-test")
	inviteOnly := fs.Bool("invite-only", false, "only invited accounts may join; @registrar is invited at once")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	if !roomAlias.MatchString(*alias) {
		fmt.Fprintln(stderr, "kwak-registrar: -alias must be lowercase letters, digits, '.', '_' or '-'")
		return 2
	}
	if !strings.HasPrefix(*hsURL, "https://") {
		fmt.Fprintln(stderr, "kwak-registrar: -homeserver must be an https:// URL")
		return 2
	}
	name := *server
	if name == "" {
		u, err := url.Parse(*hsURL)
		if err != nil || u.Hostname() == "" {
			fmt.Fprintln(stderr, "kwak-registrar: cannot read a host name from -homeserver; pass -server")
			return 2
		}
		name = u.Hostname()
	}
	if d.isTerminal(stdin) {
		fmt.Fprintln(stderr, "Paste your access token, then press Enter and Ctrl-D:")
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: read token: %v\n", err)
		return 1
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		fmt.Fprintln(stderr, "kwak-registrar: stdin must hold exactly one access token")
		return 2
	}
	c := &matrix.Client{Base: *hsURL, Token: token, HTTP: d.http}
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	bot := "@" + wire.BotLocalpart + ":" + name
	id, err := c.CreateRoom(ctx, registrar.RoomBodyFor(bot, *alias, *inviteOnly))
	if err != nil {
		fmt.Fprintf(stderr, "kwak-registrar: create the room: %v\n", registrar.ScrubError(err))
		return 1
	}
	how := ""
	if *inviteOnly {
		how = ", invite only, and invited " + bot
	}
	fmt.Fprintf(stdout, "Created #%s:%s (%s), with %s at power 50%s. Start the registrar; it joins on its own.\n", *alias, name, id, bot, how)
	return 0
}

// roomAlias is the alias name create-room accepts.
var roomAlias = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
