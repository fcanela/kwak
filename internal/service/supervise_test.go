package service_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/service/servicetest"
)

type fakeChild struct {
	mu     sync.Mutex
	exits  []error // one per run; a nil-able start error comes from startErr
	starts int
	delays []time.Duration
}

func (f *fakeChild) start(_ context.Context, stderr io.Writer) (func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.starts
	f.starts++
	io.WriteString(stderr, "run\n")
	e := f.exits[i]
	if errors.Is(e, errStart) {
		return nil, e
	}
	return func() error { return e }, nil
}

func (f *fakeChild) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.delays = append(f.delays, d)
	f.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

var errStart = errors.New("start failed")

func TestSupervisorRestarts(t *testing.T) {
	f := &fakeChild{exits: []error{errors.New("exit status 1"), errStart, nil}}
	var log bytes.Buffer
	s := service.Supervisor{Start: f.start, Log: &log, Delay: 5 * time.Second, After: f.after}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.starts != 3 || len(f.delays) != 2 || f.delays[0] != 5*time.Second {
		t.Fatalf("starts %d delays %v", f.starts, f.delays)
	}
	want := "run\nkwak supervisor: the listener stopped (exit status 1); restarting in 5s\n" +
		"run\nkwak supervisor: the listener stopped (start failed); restarting in 5s\nrun\n"
	if log.String() != want {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestSupervisorCleanExit(t *testing.T) {
	f := &fakeChild{exits: []error{nil}}
	s := service.Supervisor{Start: f.start, Log: io.Discard, Delay: time.Second, After: f.after}
	if err := s.Run(context.Background()); err != nil || f.starts != 1 || len(f.delays) != 0 {
		t.Fatalf("err %v starts %d delays %v", err, f.starts, f.delays)
	}
}

func TestSupervisorCancelDuringDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeChild{exits: []error{errors.New("exit status 2")}}
	s := service.Supervisor{Start: f.start, Log: io.Discard, Delay: time.Hour,
		After: func(time.Duration) <-chan time.Time { cancel(); return make(chan time.Time) }}
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) || f.starts != 1 {
		t.Fatalf("err %v starts %d", err, f.starts)
	}
}

func TestRotatingLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "kwak.log")
	l, err := service.OpenLog(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(l, "12345678\n")
	io.WriteString(l, "abc\n")
	io.WriteString(l, "de\n")
	io.WriteString(l, "a line longer than max\n")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p+".1"); got != "abc\nde\n" {
		t.Fatalf("kwak.log.1 = %q", got)
	}
	if got := readFile(t, p); got != "a line longer than max\n" {
		t.Fatalf("kwak.log = %q", got)
	}
	if fi, err := os.Stat(p); err != nil || (fi.Mode().Perm() != 0o600 && os.PathSeparator == '/') {
		t.Fatalf("mode: %v %v", fi.Mode(), err)
	}

	l, err = service.OpenLog(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(l, "x\n")
	l.Close()
	if got := readFile(t, p+".1"); got != "a line longer than max\n" {
		t.Fatalf("reopened over max: kwak.log.1 = %q", got)
	}
	if got := readFile(t, p); got != "x\n" {
		t.Fatalf("kwak.log = %q", got)
	}
}

func TestSupervisorStopsOnceUninstalled(t *testing.T) {
	reg := servicetest.New()
	reg.Set(service.RunValue, service.RunCommand(`C:\kwak.exe`))
	f := &fakeChild{exits: []error{errors.New("exit status 1"), errors.New("exit status 1"), nil}}
	after := func(d time.Duration) <-chan time.Time {
		reg.Delete(service.RunValue)
		return f.after(d)
	}
	s := service.Supervisor{Start: f.start, Log: io.Discard, Delay: time.Second, After: after,
		Keep: service.KeepWhileInstalled(reg)}
	if err := s.Run(context.Background()); err != nil || f.starts != 1 {
		t.Fatalf("err %v starts %d", err, f.starts)
	}
}

func TestRotatingLogRenameFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kwak.log")
	if err := os.MkdirAll(filepath.Join(p+".1", "busy"), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := service.OpenLog(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 50; i++ {
		io.WriteString(l, "0123456789\n")
	}
	data := readFile(t, p)
	if len(data) > 100+len("kwak: could not rotate the log ()\n")+200 || !strings.Contains(data, "could not rotate") {
		t.Fatalf("log grew to %d bytes: %q", len(data), data)
	}
}

func TestRotatingLogReopenFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	p := filepath.Join(dir, "kwak.log")
	l, err := service.OpenLog(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	io.WriteString(l, "123456\n")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(l, "lost\n"); err == nil {
		t.Fatal("write with no log dir succeeded")
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Small enough to fit under max next to the lost file, so no rotation is due.
	if _, err := io.WriteString(l, "ok\n"); err != nil {
		t.Fatal(err)
	}
	io.WriteString(l, "more\n")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "ok\nmore\n" {
		t.Fatalf("kwak.log = %q", got)
	}
}

func TestSuperviseRealChild(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(helperEnv, "runs")
	t.Setenv("KWAK_SERVICE_COUNT", filepath.Join(dir, "count"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "kwak.log")
	log, err := service.OpenLog(logPath, service.MaxLog)
	if err != nil {
		t.Fatal(err)
	}
	s := service.Supervisor{Start: service.ChildStarter(exe, "listen"), Log: log, Delay: 10 * time.Millisecond, After: time.After}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	log.Close()
	got := readFile(t, logPath)
	for _, want := range []string{"child run 1\n", "kwak supervisor: the listener stopped (exit status 1); restarting in 10ms\n", "child run 2\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log lacks %q:\n%s", want, got)
		}
	}
}
