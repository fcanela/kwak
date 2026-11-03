//go:build unix

package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/service"
	"github.com/fcanela/kwak/internal/wire"
)

// serve answers one connection on a real socket with reply, and returns the line it read.
func serve(t *testing.T, reply any) (path string, got <-chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "kw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "kwak.sock")
	ln, err := ipc.Listen(path, ipc.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- "accept: " + err.Error()
			return
		}
		defer c.Close()
		line, err := ipc.NewLineReader(c).Next()
		if err != nil {
			ch <- "read: " + err.Error()
			return
		}
		ch <- string(line)
		_ = ipc.WriteLine(c, reply)
	}()
	return path, ch
}

func TestShutdownAt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path, got := serve(t, wire.Ack{V: 1, OK: true})
	if err := service.ShutdownAt(ctx, path); err != nil {
		t.Fatal(err)
	}
	if line := <-got; line != `{"v":1,"op":"shutdown"}` {
		t.Fatalf("sent %s", line)
	}

	path, _ = serve(t, wire.Ack{V: 1, OK: false, Err: wire.IPCErrInternal})
	if err := service.ShutdownAt(ctx, path); err == nil || errors.Is(err, service.ErrNotRunning) {
		t.Fatalf("refused shutdown: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "kwak.sock")
	if err := service.ShutdownAt(ctx, missing); !errors.Is(err, service.ErrNotRunning) {
		t.Fatalf("no listener: %v", err)
	}
}
