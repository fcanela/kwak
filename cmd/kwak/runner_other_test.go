//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startLogged starts the test binary in mode under execRunner{Grace: grace}, with a log file as
// in production, and returns the pid the run's last process printed as "ready <pid>".
func startLogged(t *testing.T, ctx context.Context, grace time.Duration, mode, grandchild string) (func() error, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(childEnv, mode)
	t.Setenv(grandchildEnv, grandchild)
	path := filepath.Join(t.TempDir(), "run.log")
	log, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	wait, err := execRunner{Grace: grace}.Start(ctx, t.TempDir(), []string{exe}, log)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		data, _ := os.ReadFile(path)
		if f := strings.Fields(string(data)); len(f) >= 2 && f[0] == "ready" {
			pid, err := strconv.Atoi(f[1])
			if err != nil {
				t.Fatalf("log %q", data)
			}
			return wait, pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the run never got ready")
	return nil, 0
}

// gone waits until pid no longer exists. A killed grandchild is reaped by init, so it may show as
// a zombie for a moment.
func gone(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("process %d outlived its run", pid)
}

func waitFor(t *testing.T, wait func() error, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stopped run reported success")
		}
	case <-time.After(limit):
		t.Fatal("wait did not return")
	}
	return time.Since(start)
}

// A wrapper script that starts claude without exec: cancelling the run stops claude too.
func TestExecRunnerStopsTheGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wait, child := startLogged(t, ctx, 5*time.Second, "wrapper", "sleep")
	cancel()
	if took := waitFor(t, wait, 10*time.Second); took > 3*time.Second {
		t.Errorf("SIGTERM took %v", took)
	}
	gone(t, child)
}

// A child that ignores SIGTERM gets SIGKILL once the grace is over, and wait returns only then.
func TestExecRunnerKillsAStubbornChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	grace := 400 * time.Millisecond
	wait, child := startLogged(t, ctx, grace, "wrapper", "stubborn")
	cancel()
	if took := waitFor(t, wait, 10*time.Second); took < grace-100*time.Millisecond {
		t.Errorf("wait returned after %v, before the grace of %v", took, grace)
	}
	gone(t, child)
}

// A run that ignores SIGTERM itself is killed after the grace.
func TestExecRunnerKillsAStubbornRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	grace := 400 * time.Millisecond
	wait, pid := startLogged(t, ctx, grace, "stubborn", "")
	cancel()
	if took := waitFor(t, wait, 10*time.Second); took < grace-100*time.Millisecond {
		t.Errorf("wait returned after %v, before the grace of %v", took, grace)
	}
	gone(t, pid)
}
