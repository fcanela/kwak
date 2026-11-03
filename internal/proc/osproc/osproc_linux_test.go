package osproc

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/proc"
)

const helperEnv = "KWAK_OSPROC_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "sleep" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=sleep")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func TestRealChild(t *testing.T) {
	r := New()
	cmd := startChild(t)
	pid := cmd.Process.Pid

	self, parent, err := r.Lookup(pid)
	if err != nil {
		t.Fatal(err)
	}
	if self.PID != pid || self.Start == 0 {
		t.Fatalf("self = %+v", self)
	}
	if parent.PID != os.Getpid() {
		t.Fatalf("parent = %+v, want pid %d", parent, os.Getpid())
	}
	me, _, err := r.Lookup(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if self.Start < me.Start {
		t.Fatalf("child start %d before parent start %d", self.Start, me.Start)
	}
	if again, _, _ := r.Lookup(pid); again != self {
		t.Fatalf("second Lookup = %+v, want %+v", again, self)
	}
	if !proc.Alive(r, self) {
		t.Fatal("Alive false for a running child")
	}
	if proc.Alive(r, proc.ID{PID: pid, Start: self.Start + 1}) {
		t.Fatal("Alive true for another start time")
	}

	chain, err := proc.Chain(r, pid, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) < 2 || chain[0] != self || chain[1] != me {
		t.Fatalf("chain = %+v", chain)
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, _, err := r.Lookup(pid); return errors.Is(err, proc.ErrNoProcess) })
	if proc.Alive(r, self) {
		t.Fatal("Alive true for a zombie child")
	}
	_ = cmd.Wait()
	if _, _, err := r.Lookup(pid); !errors.Is(err, proc.ErrNoProcess) {
		t.Fatalf("Lookup of a reaped child: %v", err)
	}
	if proc.Alive(r, self) {
		t.Fatal("Alive true for a reaped child")
	}
}

func TestZombieIsNotRunning(t *testing.T) {
	r := New()
	cmd := startChild(t)
	pid := cmd.Process.Pid
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		return err == nil && strings.Contains(string(b), ") Z ")
	})
	if _, _, err := r.Lookup(pid); !errors.Is(err, proc.ErrNoProcess) {
		t.Fatalf("Lookup of a zombie: %v", err)
	}
}

func TestNoProcess(t *testing.T) {
	r := New()
	for _, pid := range []int{0, -1, 1 << 30} {
		if _, _, err := r.Lookup(pid); !errors.Is(err, proc.ErrNoProcess) {
			t.Errorf("Lookup(%d): %v", pid, err)
		}
	}
}

func TestStatFromFakeRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/31", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/31/stat", stat("x) (y", "S"+tail), 0o644); err != nil {
		t.Fatal(err)
	}
	self, parent, err := reader{root: root}.Lookup(31)
	if err != nil {
		t.Fatal(err)
	}
	if self != (proc.ID{PID: 31, Start: 987654}) || parent != (proc.ID{PID: 77}) {
		t.Fatalf("self %+v parent %+v", self, parent)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
