package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fcanela/kwak/internal/regclient"
	"github.com/fcanela/kwak/internal/render"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire"
)

// Waits of kwak pair.
const (
	claimRetry  = 2 * time.Second // after a network error or a quick pending answer
	claimSlack  = time.Minute     // keep polling a little past expires_at (clock skew)
	defaultWait = 30 * time.Second
)

const probeKey = "pair-test"

type pairFlags struct {
	owner, server string
}

func (f *pairFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.owner, "owner", "", "your kwak owner name, on a first pairing")
	fs.StringVar(&f.server, "server", "", "the kwak server URL (https://...), on a first pairing")
}

// Pair is kwak pair [--owner <name>] [--server <url>].
func (c *CLI) Pair(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kwak pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f pairFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if _, err := c.pair(ctx, f, stdin, stdout); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintln(stdout, "Next: kwak install.")
	return 0
}

// pair runs the pairing of spec 4. On success it has printed the "Paired as" line.
func (c *CLI) pair(ctx context.Context, f pairFlags, stdin io.Reader, stdout io.Writer) (*Config, error) {
	if c.env.IsTerminal == nil || !c.env.IsTerminal() {
		return nil, errors.New("kwak pair needs a terminal: run it yourself, in your own terminal (on Windows, PowerShell, Windows Terminal or Git Bash)")
	}
	if err := c.env.Preflight(); err != nil {
		return nil, fmt.Errorf("no session bus: run kwak pair from your desktop session (%v)", err)
	}
	if err := c.probeKeyring(); err != nil {
		return nil, fmt.Errorf("the keyring does not work: %w", err)
	}
	dir, err := c.env.ConfigDir()
	if err != nil {
		return nil, err
	}
	old, err := loadConfig(dir, c.env.InsecureHTTPForTests)
	if errors.Is(err, ErrNotPaired) {
		old, err = nil, nil
	}
	if err != nil {
		return nil, err
	}

	owner, server, err := c.who(old, f, stdin, stdout)
	if err != nil {
		return nil, err
	}
	oldKey := ""
	if old != nil {
		oldKey, err = c.env.Secrets.Get(old.KeyName())
		if errors.Is(err, secrets.ErrNotFound) {
			oldKey, err = "", nil
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read the kwak key from the keyring: %w", err)
		}
	}

	reg := c.registrar(server, nil)
	machine := c.machine()
	started, err := c.startPairing(ctx, reg, machine, oldKey, stdout)
	if err != nil {
		return nil, err
	}
	command := "pair"
	if c.env.Dev {
		// A dev build pairs for tests by default (decision 40): its agents then reach only the
		// test rooms. Sending plain "pair <code>" still makes a normal pairing.
		command = "pair test"
		fmt.Fprintln(stdout, "This is a dev build of kwak, so it suggests a test pairing.")
	}
	fmt.Fprintf(stdout, "In Element, send this to %s, in your chat with it:\n\n    %s %s\n\n"+
		"The code works once, for 10 minutes. kwak never asks you to paste a secret. Waiting...\n",
		render.Sanitize(started.Bot), command, render.Sanitize(started.Code))
	if oldKey != "" && !started.Replaces {
		fmt.Fprintln(stdout, "The registrar did not accept this machine's old key, so this is a new pairing.")
	}

	got, err := c.claim(ctx, reg, started)
	if err != nil {
		return nil, err
	}
	if err := checkClaim(got); err != nil {
		return nil, err
	}
	newReg := c.registrar(server, fixedKey(got.OwnerKey))
	if got.Owner != owner {
		msg := fmt.Sprintf("this code was sent by owner %s, not by you (%s); pairing discarded. "+
			"If you did not share the code, nobody can use it now.", got.Owner, owner)
		if err := newReg.Unpair(ctx); err != nil {
			msg += fmt.Sprintf(" The registrar did not confirm the discard (%v); the key was never stored and "+
				"expires unused in 10 minutes.", err)
		}
		return nil, errors.New(msg)
	}

	cfg := &Config{MachineConfig: wire.MachineConfig{
		Version: ConfigVersion, Server: server, ServerName: got.ServerName, Owner: got.Owner,
		Machine: machine, PairingID: got.PairingID, RoomID: got.RoomID, StandingUser: got.StandingUser,
		ResumeMax: DefaultResumeMax, Test: got.Test,
		KeyringUser: secrets.PairingKeyName(got.ServerName, got.PairingID),
	}}
	if old != nil {
		cfg.ResumeMax, cfg.ResumeExe, cfg.ThreadCeiling, cfg.extra = old.ResumeMax, old.ResumeExe, old.ThreadCeiling, old.extra
		cfg.CheckOutgoing, cfg.CheckIncoming, cfg.ResumePerHour = old.CheckOutgoing, old.CheckIncoming, old.ResumePerHour
		cfg.Resume = old.Resume
	}
	// Each pairing has its own keyring item (decision 40), so a re-pair writes a new one next to
	// the old one and removes the old one only once the registrar took the new key.
	keyName := cfg.KeyringUser
	if err := c.store(dir, keyName, got.OwnerKey, cfg); err != nil {
		return nil, errors.Join(err, c.restore(dir, keyName, old, oldKey))
	}
	if _, err := newReg.Standing(ctx); err != nil {
		if !c.oldKeyStillWorks(ctx, server, oldKey) {
			fmt.Fprintf(stdout, "The registrar did not answer the confirmation (%v), but it already refuses the old key, "+
				"so the re-pair went through and the new key stays.\n", err)
		} else {
			rerr := c.restore(dir, keyName, old, oldKey)
			what := "The new key and config were removed."
			if old != nil {
				what = "The old key and config are back."
			}
			if rerr != nil {
				what = fmt.Sprintf("Putting the old state back failed too: %v.", rerr)
			}
			return nil, fmt.Errorf("the registrar did not confirm the new key (%v). %s "+
				"The registrar drops the unconfirmed key in 10 minutes. Run kwak pair again", err, what)
		}
	}

	if old != nil {
		c.afterRepair(ctx, stdout, server, started.Bot, old, oldKey, cfg)
	}

	var rr wire.ReloadResponse
	err = c.callWait(ctx, wire.OpReload, &rr, reloadWait)
	switch {
	case err == nil && !rr.OK:
		fmt.Fprintf(stdout, "The listener did not take the new key yet (%s %s); it retries by itself.\n", rr.Err, rr.Detail)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		fmt.Fprintln(stdout, "The listener is still loading the new key; check kwak status in a minute.")
	}
	fmt.Fprintf(stdout, "Paired as %s on %s (pairing %s, key valid until %s).\n",
		cfg.Owner, cfg.Machine, cfg.PairingID, got.KeyExpiresAt.UTC().Format("2006-01-02"))
	switch {
	case cfg.Test:
		fmt.Fprintln(stdout, "This is a test pairing: its agents reach only the test rooms.")
	case c.env.Dev:
		fmt.Fprintln(stdout, "Note: this dev build got a normal pairing, so its agents join the main agents room. "+
			"For a test pairing, run kwak unpair, then kwak pair, and send \"pair test <code>\".")
	}
	return cfg, nil
}

// afterRepair runs once the registrar took the new key of a re-pair. It asks the registrar about
// the old key, removes the old keyring item when that is safe, and says when the earlier pairing
// is still live: a pairing of the other kind is a new pairing and leaves the old one as it is
// (decision 41, spec Q34).
//
// The old shared item "<server_name>/owner-key" is removed only when the registrar refuses its key:
// two folders that an older kwak paired on one machine (decision 40) both read that one item, so
// while its key works another folder may still be using it.
func (c *CLI) afterRepair(ctx context.Context, stdout io.Writer, server, bot string, old *Config, oldKey string, cfg *Config) {
	oldName := old.KeyName()
	if oldName == cfg.KeyringUser {
		return
	}
	dead, livePairing := oldKey == "", ""
	if oldKey != "" {
		me, err := c.registrar(server, fixedKey(oldKey)).Me(ctx)
		switch code := regclient.Code(err); {
		case err == nil:
			livePairing = me.PairingID
		case code == wire.ErrBadKey || code == wire.ErrOwnerRemoved:
			dead = true
		}
	}
	if oldName == secrets.OwnerKeyName(old.ServerName) && !dead {
		fmt.Fprintf(stdout, "The old shared keyring item %q (service %s) is kept: another kwak folder on this machine, "+
			"paired by an older kwak, may still use it. If none does, delete it by hand.\n", oldName, secrets.Service)
	} else if err := c.env.Secrets.Delete(oldName); err != nil && !errors.Is(err, secrets.ErrNotFound) {
		fmt.Fprintf(stdout, "The old keyring item %q (service %s) could not be deleted (%v); delete it by hand.\n",
			oldName, secrets.Service, err)
	}
	if livePairing != "" && livePairing != cfg.PairingID {
		fmt.Fprintf(stdout, "The earlier pairing %s stays live until you revoke it or it expires; to end it, send \"revoke %s\" to %s.\n",
			render.Sanitize(livePairing), render.Sanitize(livePairing), render.Sanitize(bot))
	}
}

// oldKeyStillWorks tells, after a failed POST /standing on a re-pair, whether the old key is still
// good. When the registrar refuses it as a bad key, the re-pair was confirmed (a lost answer, or the
// key confirmed by another call), so the new key must stay; the listener's own POST /standing
// confirms it if it was not.
func (c *CLI) oldKeyStillWorks(ctx context.Context, server, oldKey string) bool {
	if oldKey == "" {
		return true
	}
	_, err := c.registrar(server, fixedKey(oldKey)).Me(ctx)
	return regclient.Status(err) != 401 || regclient.Code(err) != wire.ErrBadKey
}

func (c *CLI) probeKeyring() error {
	s := c.env.Secrets
	if err := s.Set(probeKey, "ok"); err != nil {
		return err
	}
	v, err := s.Get(probeKey)
	if err == nil && v != "ok" {
		err = errors.New("read back a different value")
	}
	return errors.Join(err, s.Delete(probeKey))
}

// who returns the owner name and the server URL: from config.json when the machine is paired,
// else from the flags, then Env.Server and Env.Owner, then a prompt for the owner.
func (c *CLI) who(old *Config, f pairFlags, stdin io.Reader, stdout io.Writer) (owner, server string, err error) {
	if old != nil {
		if f.owner != "" && f.owner != old.Owner {
			return "", "", fmt.Errorf("this machine is paired as %s; run kwak unpair first to pair it as %s", old.Owner, f.owner)
		}
		if f.server != "" && strings.TrimRight(f.server, "/") != strings.TrimRight(old.Server, "/") {
			return "", "", fmt.Errorf("this machine is paired with %s; run kwak unpair first to pair it with %s", old.Server, f.server)
		}
		return old.Owner, old.Server, nil
	}
	server = f.server
	if server == "" {
		server = c.env.Server
	}
	if server == "" {
		return "", "", fmt.Errorf("no kwak server known: pass --server https://<your kwak server>, or set %s", EnvServer)
	}
	if !strings.HasPrefix(server, "https://") && !(c.env.InsecureHTTPForTests && strings.HasPrefix(server, "http://")) {
		return "", "", fmt.Errorf("the server URL must start with https://, not %q", server)
	}
	owner = f.owner
	if owner == "" {
		owner = c.env.Owner
	}
	if owner == "" {
		fmt.Fprint(stdout, "Your kwak owner name (your Matrix user name, for example fcanela): ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", "", errors.New("no owner name given")
		}
		owner = strings.TrimSpace(line)
	}
	if !wire.OwnerName.MatchString(owner) {
		return "", "", fmt.Errorf("%q is not an owner name: use lower-case letters, digits, - and _, at most 32", owner)
	}
	return owner, strings.TrimRight(server, "/"), nil
}

// machine is the sanitized host name, at most 64 characters.
func (c *CLI) machine() string {
	h, err := c.env.Hostname()
	if err != nil {
		h = ""
	}
	h = strings.TrimSpace(strings.ReplaceAll(render.Sanitize(h), "\n", " "))
	if utf8.RuneCountInString(h) > 64 {
		h = string([]rune(h)[:64])
	}
	if h == "" {
		h = "unknown"
	}
	return h
}

func (c *CLI) startPairing(ctx context.Context, reg *regclient.Client, machine, oldKey string, stdout io.Writer) (wire.PairResponse, error) {
	req := wire.PairRequest{Machine: machine, OS: runtime.GOOS, Version: wire.Version}
	r, err := reg.Pair(ctx, req, oldKey)
	var e *regclient.Error
	if errors.As(err, &e) && e.Status == 429 {
		wait := e.RetryAfter
		if wait <= 0 {
			wait = defaultWait
		}
		fmt.Fprintf(stdout, "kwak pair is rate limited; waiting %d s\n", int(wait.Seconds()))
		if err := c.env.Sleep(ctx, wait); err != nil {
			return r, err
		}
		r, err = reg.Pair(ctx, req, oldKey)
	}
	if err != nil {
		return r, fmt.Errorf("could not start the pairing: %w", err)
	}
	if r.Code == "" || r.Claim == "" {
		return r, errors.New("the registrar sent an incomplete answer")
	}
	return r, nil
}

// claim long-polls until the code was sent, the pairing expired, or ctx ends.
func (c *CLI) claim(ctx context.Context, reg *regclient.Client, p wire.PairResponse) (wire.ClaimResponse, error) {
	deadline := p.ExpiresAt.Add(claimSlack)
	for {
		start := c.env.Now()
		r, err := reg.Claim(ctx, p.Claim)
		switch {
		case err == nil && r.Status == wire.ClaimPaired:
			return r, nil
		case err == nil && r.Status != wire.ClaimPending:
			return r, fmt.Errorf("the registrar answered the claim with status %q", r.Status)
		case regclient.Code(err) == wire.ErrExpired:
			return r, errors.New("pairing expired; run kwak pair again")
		case ctx.Err() != nil:
			return r, ctx.Err()
		case err != nil && regclient.Status(err) >= 400 && regclient.Status(err) < 500:
			return r, fmt.Errorf("the registrar refused the claim: %w", err)
		}
		now := c.env.Now()
		if !p.ExpiresAt.IsZero() && now.After(deadline) {
			return r, errors.New("pairing expired; run kwak pair again")
		}
		if err != nil || now.Sub(start) < claimRetry {
			if err := c.env.Sleep(ctx, claimRetry); err != nil {
				return r, err
			}
		}
	}
}

func checkClaim(r wire.ClaimResponse) error {
	switch {
	case !strings.HasPrefix(r.OwnerKey, "kwak_ok_"):
		return errors.New("the registrar sent no owner key")
	case r.ServerName == "" || strings.ContainsAny(r.ServerName, "/\\ \t\n") || r.PairingID == "":
		return errors.New("the registrar sent an incomplete pairing")
	case !wire.OwnerName.MatchString(r.Owner):
		return fmt.Errorf("the registrar sent a bad owner name %q", r.Owner)
	}
	return nil
}

// store writes the key, reads it back, then writes config.json.
func (c *CLI) store(dir, keyName, key string, cfg *Config) error {
	if err := c.env.Secrets.Set(keyName, key); err != nil {
		return fmt.Errorf("could not write the key to the keyring: %w", err)
	}
	if v, err := c.env.Secrets.Get(keyName); err != nil || v != key {
		return fmt.Errorf("the keyring did not give the key back (%v)", err)
	}
	if err := cfg.Save(dir); err != nil {
		return fmt.Errorf("could not write %s: %w", ConfigFile, err)
	}
	return nil
}

// restore puts the state from before the pairing back: the old key and config.json, or neither
// on a first pairing.
func (c *CLI) restore(dir, keyName string, old *Config, oldKey string) error {
	var errs []error
	if old == nil {
		if err := c.env.Secrets.Delete(keyName); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			errs = append(errs, err)
		}
		if err := os.Remove(filepath.Join(dir, ConfigFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	oldName := old.KeyName()
	if oldName != keyName || oldKey == "" {
		if err := c.env.Secrets.Delete(keyName); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	if oldKey != "" {
		if err := c.env.Secrets.Set(oldName, oldKey); err != nil {
			errs = append(errs, err)
		}
	}
	if err := old.Save(dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
