package service

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// Commands are kwak install and kwak uninstall over a Manager. Exe is Executable in production.
type Commands struct {
	Manager Manager
	Exe     func() (string, error)
}

func parse(name string, args []string, stderr io.Writer) bool {
	fs := flag.NewFlagSet("kwak "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, "usage: kwak %s\n", name) }
	if err := fs.Parse(args); err != nil {
		return false
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return false
	}
	return true
}

// Install is kwak install: it writes the user service for this binary and (re)starts it.
func (c Commands) Install(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if !parse("install", args, stderr) {
		return 2
	}
	exe, err := c.Exe()
	if err != nil {
		fmt.Fprintf(stderr, "kwak install: cannot find the kwak binary: %v\n", err)
		return 1
	}
	if err := c.Manager.Install(exe); err != nil {
		fmt.Fprintf(stderr, "kwak install: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "kwak install: the listener runs as a user service from %s.\n", exe)
	if st, err := c.Manager.Status(); err == nil {
		fmt.Fprintf(stdout, "Service status: %s.\n", st)
	}
	return 0
}

// Uninstall is kwak uninstall: it stops and removes the user service. The pairing stays.
func (c Commands) Uninstall(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if !parse("uninstall", args, stderr) {
		return 2
	}
	err := c.Manager.Uninstall()
	switch {
	case errors.Is(err, ErrNotInstalled):
		fmt.Fprintln(stdout, "kwak uninstall: no kwak user service is installed. Nothing to do.")
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "kwak uninstall: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "kwak uninstall: the listener is stopped and its user service is removed.")
	fmt.Fprintln(stdout, "The pairing and the keyring item stay. Run kwak unpair to remove them.")
	return 0
}
