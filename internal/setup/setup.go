// Package setup is `kwak setup claude|codex [--write|--remove]` and `kwak doctor`. setup prints
// the harness config (spec 6.2, 6.3); for Codex it can also merge it into $CODEX_HOME or remove
// it. It never writes Claude Code's config. doctor only reads.
package setup

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fcanela/kwak/internal/atomicfile"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/wire"
)

// MainFunc is the shape of a kwak subcommand.
type MainFunc func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// EnvCodexHome names Codex's config dir; the default is ~/.codex.
const EnvCodexHome = "CODEX_HOME"

// AllowRule is the Claude Code permission rule that lets kwak's tools run without a prompt.
const AllowRule = "mcp__plugin_kwak_kwak__*"

// Print writes the harness steps that `kwak setup <h>` prints. Bare `kwak setup` calls it too.
func Print(w io.Writer, h wire.Harness) error {
	switch h {
	case wire.HarnessClaude:
		_, err := io.WriteString(w, claudeSteps())
		return err
	case wire.HarnessCodex:
		exe, err := executable()
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, codexSteps(exe, runtime.GOOS))
		return err
	}
	return fmt.Errorf("setup: unknown harness %q", h)
}

func executable() (string, error) {
	exe, err := paths.Executable()
	if err != nil {
		return "", fmt.Errorf("setup: cannot find the kwak binary: %w", err)
	}
	return exe, nil
}

func claudeSteps() string {
	return "# Claude Code: install the kwak plugin, pinned to this kwak's version.\n" +
		"claude plugin marketplace add fcanela/kwak#v" + wire.Version + "\n" +
		"claude plugin install kwak@kwak\n" +
		"# then add to ~/.claude/settings.json, inside \"permissions\":\n" +
		`"allow": ["` + AllowRule + `"]` + "\n" +
		"# kwak must be on the PATH Claude Code starts with. Check with: kwak doctor\n"
}

func codexSteps(exe, goos string) string {
	return "# Codex: add this block to $CODEX_HOME/config.toml (default ~/.codex/config.toml):\n\n" +
		configBlock(exe) + "\n" +
		"# and merge this into $CODEX_HOME/hooks.json:\n\n" +
		string(hooksDoc(exe, goos)) + "\n" +
		"# Or let kwak merge both: kwak setup codex --write\n" +
		trustStep
}

const trustStep = "# Then run codex (the TUI) once and choose \"Review hooks\" or \"Trust all and continue\".\n" +
	"# Until then codex exec skips kwak's hooks without a word. Tools still work.\n"

// Main is `kwak setup claude|codex [--write|--remove]`.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return mainFor(false, args, stdout, stderr)
}

// DevMain is Main in a dev build (make dev): --write and --remove change Codex's config only
// when CODEX_HOME is set, so a dev build never points the real ~/.codex at itself (decision 40).
func DevMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return mainFor(true, args, stdout, stderr)
}

func mainFor(dev bool, args []string, stdout, stderr io.Writer) int {
	home := os.Getenv(EnvCodexHome)
	explicit := home != ""
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	exe, err := executable()
	if err != nil {
		fmt.Fprintln(stderr, "kwak setup:", err)
		return 1
	}
	return run(args, stdout, stderr, env{codexHome: home, exe: exe, goos: runtime.GOOS, dev: dev, homeSet: explicit})
}

type env struct {
	codexHome string
	exe       string
	goos      string
	dev       bool // a dev build
	homeSet   bool // CODEX_HOME was set, not the ~/.codex default
}

// textDevCodexHome is why a dev build refuses to write the default Codex home.
const textDevCodexHome = "this is a dev build, so it changes Codex's config only in a CODEX_HOME you set " +
	"(for example CODEX_HOME=$HOME/.codex-kwak-dev), never in ~/.codex, which your real kwak uses"

const usage = "usage: kwak setup claude | kwak setup codex [--write | --remove] [--exe <path>]"

func run(args []string, stdout, stderr io.Writer, e env) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("kwak setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	write := fs.Bool("write", false, "")
	remove := fs.Bool("remove", false, "")
	exe := fs.String("exe", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || (*write && *remove) {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	mode := ""
	switch {
	case *write:
		mode = "--write"
	case *remove:
		mode = "--remove"
	}
	if *exe != "" {
		// --exe names the kwak the config starts, for a setup that knows a steadier path than
		// this binary's, such as a package manager's profile link.
		if !filepath.IsAbs(*exe) {
			fmt.Fprintf(stderr, "kwak setup: --exe %q is not an absolute path\n", *exe)
			return 2
		}
		e.exe = filepath.Clean(*exe)
	}
	switch wire.Harness(args[0]) {
	case wire.HarnessClaude:
		if mode != "" || *exe != "" {
			fmt.Fprintln(stderr, "kwak setup claude only prints the steps. It never writes Claude Code's settings.")
			return 2
		}
		fmt.Fprint(stdout, claudeSteps())
		return 0
	case wire.HarnessCodex:
		if e.dev && mode != "" && !e.homeSet {
			fmt.Fprintln(stderr, "kwak setup codex:", textDevCodexHome)
			return 1
		}
		var err error
		switch mode {
		case "":
			fmt.Fprint(stdout, codexSteps(e.exe, e.goos))
		case "--write":
			err = writeCodex(stdout, e)
		case "--remove":
			err = removeCodex(stdout, e)
		}
		if err != nil {
			fmt.Fprintln(stderr, "kwak setup codex:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// target follows path when it is a symlink (dotfile managers keep config there), so a write
// goes to the file the link points at and the link stays.
func target(path string) (string, bool, error) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return path, false, nil
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", true, fmt.Errorf("%s is a link kwak cannot follow: %w", path, err)
	}
	return real, true, nil
}

// writeAtomic writes data through a temp file and a rename, keeping the old file's mode.
func writeAtomic(path string, data []byte) error {
	path, _, err := target(path)
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return atomicfile.Write(path, data, mode)
}

func writeCodex(w io.Writer, e env) error {
	if e.codexHome == "" {
		return errors.New("no Codex home; set CODEX_HOME")
	}
	cfgPath := filepath.Join(e.codexHome, "config.toml")
	hooksPath := filepath.Join(e.codexHome, "hooks.json")
	cfgOld, err := readOptional(cfgPath)
	if err != nil {
		return err
	}
	hooksOld, err := readOptional(hooksPath)
	if err != nil {
		return err
	}
	cfgNew, cres, err := mergeConfig(string(cfgOld), e.exe)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	hooksNew, ch, err := mergeHooks(hooksOld, e.exe, e.goos)
	if err != nil {
		return fmt.Errorf("%s: %w", hooksPath, err)
	}
	if err := os.MkdirAll(e.codexHome, 0o700); err != nil {
		return err
	}
	if cres == configAdded || cres == configChanged {
		if err := writeAtomic(cfgPath, []byte(cfgNew)); err != nil {
			return err
		}
	}
	if ch.any() {
		if err := writeAtomic(hooksPath, hooksNew); err != nil {
			return err
		}
	}
	switch cres {
	case configAdded:
		fmt.Fprintf(w, "Added the kwak MCP server to %s.\n", cfgPath)
	case configChanged:
		fmt.Fprintf(w, "Updated the kwak MCP server in %s.\n", cfgPath)
	case configForeign:
		fmt.Fprintf(w, "%s already has [mcp_servers.kwak], so kwak left it alone.\n", cfgPath)
	default:
		fmt.Fprintf(w, "%s already has the kwak MCP server.\n", cfgPath)
	}
	if len(ch.Added) > 0 {
		fmt.Fprintf(w, "Added kwak hooks to %s: %s.\n", hooksPath, strings.Join(ch.Added, ", "))
	}
	if len(ch.Changed) > 0 {
		fmt.Fprintf(w, "Changed kwak hooks in %s: %s. Codex marks a changed hook as Modified and skips it until you trust it again.\n",
			hooksPath, strings.Join(ch.Changed, ", "))
	}
	if !ch.any() {
		fmt.Fprintf(w, "%s already has kwak's hooks.\n", hooksPath)
		return nil
	}
	fmt.Fprint(w, strings.ReplaceAll(strings.TrimPrefix(trustStep, "# "), "\n# ", "\n"))
	return nil
}

func removeCodex(w io.Writer, e env) error {
	if e.codexHome == "" {
		return errors.New("no Codex home; set CODEX_HOME")
	}
	cfgPath := filepath.Join(e.codexHome, "config.toml")
	hooksPath := filepath.Join(e.codexHome, "hooks.json")
	cfgOld, err := readOptional(cfgPath)
	if err != nil {
		return err
	}
	hooksOld, err := readOptional(hooksPath)
	if err != nil {
		return err
	}
	cfgNew, cres, err := removeConfig(string(cfgOld))
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	hooksNew, ch, err := removeHooks(hooksOld)
	if err != nil {
		return fmt.Errorf("%s: %w", hooksPath, err)
	}
	switch cres {
	case configRemoved:
		if strings.TrimSpace(cfgNew) == "" {
			err = removeFile(cfgPath, nil)
		} else {
			err = writeAtomic(cfgPath, []byte(cfgNew))
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Removed the kwak MCP server from %s.\n", cfgPath)
	case configForeign:
		fmt.Fprintf(w, "%s has [mcp_servers.kwak] outside kwak's markers. kwak did not write it, so remove it by hand.\n", cfgPath)
	default:
		fmt.Fprintf(w, "%s has no kwak MCP server.\n", cfgPath)
	}
	if !ch.any() {
		fmt.Fprintf(w, "%s has no kwak hooks.\n", hooksPath)
		return nil
	}
	if hooksNew == nil {
		err = removeFile(hooksPath, indent(map[string]any{"description": HooksDescription, "hooks": map[string]any{}}))
	} else {
		err = writeAtomic(hooksPath, hooksNew)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Removed kwak hooks from %s: %s.\n", hooksPath, strings.Join(ch.Removed, ", "))
	return nil
}

// removeFile deletes a file kwak made. A symlink stays, and its target gets empty instead.
func removeFile(path string, empty []byte) error {
	_, link, err := target(path)
	if err != nil {
		return err
	}
	if link {
		return writeAtomic(path, empty)
	}
	return os.Remove(path)
}
