package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/fcanela/kwak/internal/paths"
)

// Supervisor defaults (spec 7.3).
const (
	LogName      = "kwak.log"
	MaxLog       = 5 << 20
	RestartDelay = 5 * time.Second
)

// Supervisor runs the listener as a child and restarts it Delay after any failed run, until a
// run exits 0, ctx ends, or Keep (when set) says before a restart that kwak was uninstalled.
// Start launches one run with stderr as its standard error.
type Supervisor struct {
	Start func(ctx context.Context, stderr io.Writer) (wait func() error, err error)
	Log   io.Writer
	Delay time.Duration
	After func(time.Duration) <-chan time.Time
	Keep  func() bool
}

// Run returns nil once a run exits 0 or Keep says stop, or ctx's error.
func (s Supervisor) Run(ctx context.Context) error {
	for {
		wait, err := s.Start(ctx, s.Log)
		if err == nil {
			if err = wait(); err == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(s.Log, "kwak supervisor: the listener stopped (%v); restarting in %s\n", err, s.Delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.After(s.Delay):
		}
		if s.Keep != nil && !s.Keep() {
			fmt.Fprintln(s.Log, "kwak supervisor: kwak was uninstalled; not restarting")
			return nil
		}
	}
}

// RotatingLog is an append-only file that moves itself to <path>.1 before it would grow past max.
type RotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// OpenLog opens path for appending, creating its directory (0700) and the file (0600).
func OpenLog(path string, max int64) (*RotatingLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &RotatingLog{path: path, max: max}
	if err := l.open(0); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *RotatingLog) open(flag int) error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|flag, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// Write implements io.Writer. The file is closed before the rename, which Windows needs. When the
// rename fails (on Windows, another process holds a log file open), the log starts again empty, so
// it never grows past max. If it cannot be reopened, Write fails and the next Write tries again.
func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > l.max {
		l.f.Close()
		l.f = nil
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			if err := l.open(os.O_TRUNC); err != nil {
				return 0, err
			}
			n, _ := fmt.Fprintf(l.f, "kwak: could not rotate the log (%v); started it again\n", err)
			l.size += int64(n)
		}
	}
	if l.f == nil {
		if err := l.open(0); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// LogPath is the supervisor's log, kwak.log in the state dir (%LOCALAPPDATA%\kwak on Windows).
func LogPath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, LogName), nil
}

// Supervise is kwak listen --supervise: it runs "exe listen" as a hidden child whose stderr is
// the rotating log at logPath, and restarts it 5 s after a non-zero exit. It returns the exit code.
func Supervise(ctx context.Context, exe, logPath string) int {
	log, err := OpenLog(logPath, MaxLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kwak supervisor: cannot open the log: %v\n", err)
		return 1
	}
	defer log.Close()
	s := Supervisor{Start: ChildStarter(exe, "listen"), Log: log, Delay: RestartDelay, After: time.After, Keep: keepSupervising()}
	if err := s.Run(ctx); err != nil {
		return 1
	}
	return 0
}

// ChildStarter starts exe with args as a hidden child, killed when ctx ends.
func ChildStarter(exe string, args ...string) func(context.Context, io.Writer) (func() error, error) {
	return func(ctx context.Context, stderr io.Writer) (func() error, error) {
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Stderr = stderr
		cmd.SysProcAttr = childAttr()
		cmd.WaitDelay = time.Second
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd.Wait, nil
	}
}

// Detach is kwak listen --detach: it starts "exe listen --supervise" with no console and returns.
func Detach(exe string) error {
	cmd := exec.Command(exe, "listen", "--supervise")
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
