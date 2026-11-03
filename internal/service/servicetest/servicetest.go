// Package servicetest records what a service.Manager asks of the OS: commands, Run key values
// and shutdown sends, in one ordered list.
package servicetest

import (
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fcanela/kwak/internal/service"
)

// Recorder is a service.Runner, a service.Registry and a shutdown sender. Calls succeed with no
// output unless On scripted them. It is safe for concurrent use.
type Recorder struct {
	mu       sync.Mutex
	calls    []string
	answers  map[string]answer
	queued   map[string][]answer
	values   map[string]string
	shutdown error
}

type answer struct {
	out string
	err error
}

// New returns an empty Recorder with an empty Run key.
func New() *Recorder {
	return &Recorder{answers: map[string]answer{}, queued: map[string][]answer{}, values: map[string]string{}}
}

// Line formats a command the way Calls lists it: arguments with spaces or quotes are quoted.
func Line(name string, args ...string) string {
	parts := []string{quote(name)}
	for _, a := range args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

func quote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"'\\") {
		return strconv.Quote(s)
	}
	return s
}

// On makes the command line (as Line formats it) answer out and err from now on.
func (r *Recorder) On(line, out string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[line] = answer{out, err}
}

// Once makes the next call of line answer out and err, before any answer On set. Several Once
// calls queue up.
func (r *Recorder) Once(line, out string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queued[line] = append(r.queued[line], answer{out, err})
}

// Sleep implements service.Sleeper: it records "sleep <d>" and returns at once.
func (r *Recorder) Sleep(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("sleep " + d.String())
}

// ShutdownErr makes Shutdown return err (service.ErrNotRunning for no listener).
func (r *Recorder) ShutdownErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shutdown = err
}

// Calls returns the calls so far and forgets them.
func (r *Recorder) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls
	r.calls = nil
	return c
}

// Value is the Run key value name, as Set left it.
func (r *Recorder) Value(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.values[name]
	return v, ok
}

func (r *Recorder) record(line string) {
	r.calls = append(r.calls, line)
}

// Run implements service.Runner.
func (r *Recorder) Run(name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := Line(name, args...)
	r.record(line)
	a := r.answers[line]
	if q := r.queued[line]; len(q) > 0 {
		a, r.queued[line] = q[0], q[1:]
	}
	return []byte(a.out), a.err
}

// Get implements service.Registry.
func (r *Recorder) Get(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record(Line("reg-get", name))
	v, ok := r.values[name]
	if !ok {
		return "", fs.ErrNotExist
	}
	return v, nil
}

// Set implements service.Registry.
func (r *Recorder) Set(name, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record(Line("reg-set", name, value))
	r.values[name] = value
	return nil
}

// Delete implements service.Registry.
func (r *Recorder) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record(Line("reg-delete", name))
	if _, ok := r.values[name]; !ok {
		return fs.ErrNotExist
	}
	delete(r.values, name)
	return nil
}

// Shutdown is the shutdown sender for service.RunKey.
func (r *Recorder) Shutdown() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("ipc-shutdown")
	return r.shutdown
}

var (
	_ service.Runner   = (*Recorder)(nil)
	_ service.Sleeper  = (*Recorder)(nil)
	_ service.Registry = (*Recorder)(nil)
)
