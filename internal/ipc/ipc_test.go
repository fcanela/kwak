package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortDir is a temp dir with a short name, so socket paths stay under the limit.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "kw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

type msg struct {
	V    int    `json:"v"`
	Op   string `json:"op"`
	Text string `json:"text,omitempty"`
}

func TestRoundTrip(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s", "kwak.sock")
	ln, err := Listen(sock, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		line, err := NewLineReader(c).Next()
		if err != nil {
			done <- err
			return
		}
		_, err = c.Write(append(line, '\n'))
		done <- err
	}()
	c, err := Dial(ctx(t), sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := WriteLine(c, msg{V: 1, Op: "status", Text: "<a&b>"}); err != nil {
		t.Fatal(err)
	}
	got, err := NewLineReader(c).Next()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":1,"op":"status","text":"<a&b>"}`; string(got) != want {
		t.Fatalf("echo = %s, want %s", got, want)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCall(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s", "kwak.sock")
	if err := Call(ctx(t), sock, time.Second, msg{V: 1}, &msg{}); !errors.Is(err, ErrDial) {
		t.Fatalf("no listener: %v", err)
	}
	ln, err := Listen(sock, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if line, err := NewLineReader(c).Next(); err == nil {
			c.Write(append(bytes.Replace(line, []byte(`"status"`), []byte(`"ok"`), 1), '\n'))
		}
	}()
	var got msg
	if err := Call(ctx(t), sock, time.Second, msg{V: 1, Op: "status"}, &got); err != nil || got.Op != "ok" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestStaleSocket(t *testing.T) {
	dir := filepath.Join(shortDir(t), "s")
	sock := filepath.Join(dir, "kwak.sock")
	if err := RemoveStale(sock); err != nil {
		t.Fatalf("missing socket: %v", err)
	}

	old, err := Listen(sock, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveStale(sock); !errors.Is(err, ErrInUse) {
		t.Fatalf("live socket: err = %v, want ErrInUse", err)
	}
	old.(*filtered).Listener.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("stale socket gone: %v", err)
	}

	if _, err := Listen(sock, Policy{}); err == nil {
		t.Fatal("Listen removed a stale socket by itself")
	}
	unlock, err := Lock(filepath.Join(dir, "listen.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := RemoveStale(sock); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(sock, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStale(file); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("plain file: err = %v, want ErrUnsafe", err)
	}
	if _, err := os.Lstat(file); err != nil {
		t.Fatal("removed a plain file")
	}
}

func TestPathTooLong(t *testing.T) {
	dir := shortDir(t)
	long := filepath.Join(dir, strings.Repeat("a", maxPath))
	if _, err := Listen(long, Policy{}); !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("err = %v, want ErrPathTooLong", err)
	}
	if _, err := os.Stat(filepath.Dir(long)); err != nil {
		t.Fatal(err)
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listen.lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock: err = %v, want ErrLocked", err)
	}
	release()
	release()
	again, err := Lock(path)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	again()
}

func TestLockConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listen.lock")
	var wins, locked atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	releases := make(chan func(), 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rel, err := Lock(path)
			switch {
			case err == nil:
				wins.Add(1)
				releases <- rel
			case errors.Is(err, ErrLocked):
				locked.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(releases)
	for rel := range releases {
		rel()
	}
	if wins.Load() != 1 || locked.Load() != 15 {
		t.Fatalf("wins = %d, locked = %d", wins.Load(), locked.Load())
	}
}

func TestLineReader(t *testing.T) {
	exact := bytes.Repeat([]byte("x"), MaxLine)
	in := "a\n\nbb\n" + string(exact) + "\nlast"
	r := NewLineReader(strings.NewReader(in))
	for _, want := range []string{"a", "", "bb", string(exact), "last"} {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("want %.10q: %v", want, err)
		}
		if string(got) != want {
			t.Fatalf("got %.10q (%d bytes), want %.10q (%d bytes)", got, len(got), want, len(want))
		}
	}
	for range 2 {
		if _, err := r.Next(); err != io.EOF {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	}
	if _, err := NewLineReader(strings.NewReader("")).Next(); err != io.EOF {
		t.Fatalf("empty input: err = %v", err)
	}
}

func TestLineTooLong(t *testing.T) {
	over := string(bytes.Repeat([]byte("x"), MaxLine+1))
	for _, in := range []string{over + "\nok\n", over} {
		r := NewLineReader(strings.NewReader(in))
		if _, err := r.Next(); !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("err = %v, want ErrLineTooLong", err)
		}
		if _, err := r.Next(); !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("not sticky: %v", err)
		}
	}
}

func TestLineCopy(t *testing.T) {
	r := NewLineReader(strings.NewReader("one\ntwo\n"))
	a, _ := r.Next()
	b, _ := r.Next()
	if string(a) != "one" || string(b) != "two" {
		t.Fatalf("a = %q, b = %q", a, b)
	}
}

type countWriter struct {
	bytes.Buffer
	writes int
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func TestWriteLine(t *testing.T) {
	var w countWriter
	if err := WriteLine(&w, map[string]any{"v": 1, "text": "a\nb"}); err != nil {
		t.Fatal(err)
	}
	if got, want := w.String(), `{"text":"a\nb","v":1}`+"\n"; got != want || w.writes != 1 {
		t.Fatalf("got %q in %d writes, want %q", got, w.writes, want)
	}

	w.Reset()
	raw := struct {
		Args json.RawMessage `json:"args"`
	}{json.RawMessage("{\n  \"a\": 1\n}")}
	if err := WriteLine(&w, raw); err != nil {
		t.Fatal(err)
	}
	if got := w.String(); got != `{"args":{"a":1}}`+"\n" {
		t.Fatalf("raw message not compacted: %q", got)
	}

	pad := MaxLine - len(`""`)
	w.Reset()
	if err := WriteLine(&w, strings.Repeat("x", pad)); err != nil {
		t.Fatalf("exactly MaxLine: %v", err)
	}
	if w.Len() != MaxLine+1 {
		t.Fatalf("wrote %d bytes", w.Len())
	}
	w.Reset()
	if err := WriteLine(&w, strings.Repeat("x", pad+1)); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
	if w.Len() != 0 {
		t.Fatal("wrote an oversize line")
	}
}
