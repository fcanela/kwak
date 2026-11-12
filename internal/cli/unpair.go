package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/regclient"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/wire"
)

// How long unpair waits for the listener to free listen.lock, and then for the state dir to go
// (on Windows the supervisor closes kwak.log a moment after the listener exits).
const (
	stopTries = 20
	stopPoll  = 500 * time.Millisecond
)

// Unpair is kwak unpair [--force]: revoke this pairing at the registrar, delete the key and
// config.json, stop the listener, then delete the state dir (spec 7.4). Without --force a
// registrar that cannot be reached keeps everything in place.
func (c *CLI) Unpair(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kwak unpair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "delete the local key, config and state even if the registrar cannot be told")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	cdir, err := c.env.ConfigDir()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	sdir, err := c.env.StateDir()
	if err != nil {
		return fail(stderr, "%v", err)
	}
	cfg, err := loadConfig(cdir, c.env.InsecureHTTPForTests)
	if errors.Is(err, ErrNotPaired) {
		fmt.Fprintln(stdout, "This machine is not paired.")
		return 0
	}
	if err != nil && !*force {
		return fail(stderr, "%v. Run kwak unpair --force to delete the local files anyway", err)
	}
	var serverName, pairingID, keyName string
	if cfg != nil {
		serverName, pairingID, keyName = cfg.ServerName, cfg.PairingID, cfg.KeyName()
	} else {
		var keyringUser string
		serverName, pairingID, keyringUser = looseIDs(cdir)
		keyName = secrets.KeyName(serverName, pairingID, keyringUser)
	}

	told := false
	if cfg != nil {
		key, kerr := c.env.Secrets.Get(keyName)
		switch {
		case kerr != nil && !*force:
			return fail(stderr, "cannot read the kwak key from the keyring: %v. Nothing was deleted. "+
				"Run kwak unpair --force to delete the local files anyway", kerr)
		case kerr == nil:
			uerr := c.registrar(cfg.Server, fixedKey(key)).Unpair(ctx)
			switch code := regclient.Code(uerr); {
			case uerr == nil:
				told = true
			case code == wire.ErrBadKey || code == wire.ErrOwnerRemoved:
				told = true
				fmt.Fprintln(stdout, "The registrar no longer accepts this key.")
			case !*force:
				return fail(stderr, "could not tell the registrar (%v). The key is kept. "+
					"Try again later, or run kwak unpair --force", uerr)
			}
		}
	}
	keyKnown := serverName != ""
	if keyKnown {
		err := c.env.Secrets.Delete(keyName)
		switch {
		case err == nil || errors.Is(err, secrets.ErrNotFound):
		case !*force:
			return fail(stderr, "could not delete the key from the keyring: %v", err)
		default:
			fmt.Fprintf(stderr, "kwak: could not delete the key from the keyring (%v); delete %q (service %s) by hand.\n",
				err, keyName, secrets.Service)
		}
	}
	failed := false
	if err := os.Remove(filepath.Join(cdir, ConfigFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		if !*force {
			return fail(stderr, "could not delete %s: %v", ConfigFile, err)
		}
		fmt.Fprintf(stderr, "kwak: could not delete %s: %v\n", filepath.Join(cdir, ConfigFile), err)
		failed = true
	}
	stateErr := c.stopListener(ctx, sdir)
	if stateErr == nil {
		stateErr = c.removeStateDir(ctx, sdir)
	}

	what := "The key, the config and the state are deleted."
	switch {
	case !keyKnown && stateErr != nil:
		what = "The config is deleted."
	case !keyKnown:
		what = "The config and the state are deleted."
	case stateErr != nil:
		what = "The key and the config are deleted."
	}
	if cfg != nil {
		fmt.Fprintf(stdout, "Unpaired %s (pairing %s). %s\n", cfg.Machine, cfg.PairingID, what)
	} else {
		fmt.Fprintf(stdout, "Deleted the local files. %s\n", what)
	}
	if !keyKnown {
		fmt.Fprintf(stdout, "The server name is unknown, so the kwak key may still be in your keyring: "+
			"service %s, user <server name>/owner-key. Delete it by hand.\n", secrets.Service)
	}
	switch {
	case told:
	case pairingID != "":
		bot := "@registrar"
		if serverName != "" {
			bot += ":" + serverName
		}
		fmt.Fprintf(stdout, "The registrar was not told. Send this to %s: revoke %s\n", bot, pairingID)
	default:
		fmt.Fprintln(stdout, "The registrar was not told and the pairing id is unknown: "+
			"send machines to @registrar to find it, then revoke <id>.")
	}
	if stateErr != nil {
		fmt.Fprintf(stderr, "kwak: %v\n", stateErr)
		return 1
	}
	fmt.Fprintln(stdout, "The kwak listener is not running. Next: kwak uninstall, or kwak setup to pair again.")
	if failed {
		return 1
	}
	return 0
}

// looseIDs reads server_name, pairing_id and keyring_user from a config.json that loadConfig
// refused, so unpair --force can still delete the key and print the revoke line. keyring_user is
// only compared with the name kwak derives (secrets.KeyName), never used as it is.
func looseIDs(dir string) (serverName, pairingID, keyringUser string) {
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return "", "", ""
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return "", "", ""
	}
	sn, _ := m["server_name"].(string)
	id, _ := m["pairing_id"].(string)
	keyringUser, _ = m["keyring_user"].(string)
	if !plainID(sn) {
		sn = ""
	}
	if !plainID(id) {
		id = ""
	}
	return sn, id, keyringUser
}

func plainID(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':') {
			return false
		}
	}
	return true
}

// stopListener asks a running listener to exit and waits until it frees listen.lock. The
// listener writes state.json before it lets go of the lock, so after this nothing writes to the
// state dir.
func (c *CLI) stopListener(ctx context.Context, sdir string) error {
	var ack wire.Ack
	_ = c.call(ctx, wire.OpShutdown, &ack)
	for i := 1; ; i++ {
		release, err := ipc.Lock(filepath.Join(sdir, "listen.lock"))
		switch {
		case err == nil:
			release()
			return nil
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case !errors.Is(err, ipc.ErrLocked):
			return fmt.Errorf("could not check that the listener stopped: %v. The state dir %s is kept", err, sdir)
		}
		if i == stopTries || c.env.Sleep(ctx, stopPoll) != nil {
			return fmt.Errorf("the kwak listener did not stop, so the state dir %s is kept. "+
				"Run kwak uninstall, then delete it by hand", sdir)
		}
	}
}

// removeStateDir deletes dir and everything in it, retrying a while, unless stateDirGuard
// refuses it.
func (c *CLI) removeStateDir(ctx context.Context, dir string) error {
	home, _ := os.UserHomeDir()
	if err := stateDirGuard(dir, home); err != nil {
		return err
	}
	var err error
	for i := 1; ; i++ {
		if err = os.RemoveAll(filepath.Clean(dir)); err == nil {
			return nil
		}
		if i == stopTries || c.env.Sleep(ctx, stopPoll) != nil {
			return fmt.Errorf("could not delete the state dir: %w", err)
		}
	}
}

// stateDirGuard refuses a path that cannot be a kwak state dir: relative, a root, home, or a
// directory that holds home.
func stateDirGuard(dir, home string) error {
	clean := filepath.Clean(dir)
	bad := !filepath.IsAbs(clean) || filepath.Dir(clean) == clean
	if home != "" {
		if rel, err := filepath.Rel(clean, filepath.Clean(home)); err == nil && filepath.IsLocal(rel) {
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("refusing to delete the state dir %q", dir)
	}
	return nil
}
