//go:build linux || darwin

package ipc

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPeerCred(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s", "kwak.sock")
	ln, err := Listen(sock, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Error(err)
		}
		accepted <- c
	}()
	c, err := Dial(ctx(t), sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := <-accepted
	if s == nil {
		t.FailNow()
	}
	defer s.Close()
	for _, conn := range []net.Conn{s, c} {
		pid, ok, err := PeerPID(conn)
		if err != nil || !ok || pid != os.Getpid() {
			t.Fatalf("PeerPID = %d, %v, %v; want %d", pid, ok, err, os.Getpid())
		}
		uid, ok, err := PeerUID(conn)
		if err != nil || !ok || uid != os.Getuid() {
			t.Fatalf("PeerUID = %d, %v, %v; want %d", uid, ok, err, os.Getuid())
		}
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, ok, err := PeerUID(a); ok || err != nil {
		t.Fatalf("pipe: ok = %v, err = %v", ok, err)
	}
	if _, ok, err := PeerPID(a); ok || err != nil {
		t.Fatalf("pipe: ok = %v, err = %v", ok, err)
	}
}

func TestAllowedUIDs(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s", "kwak.sock")
	ln, err := Listen(sock, Policy{AllowedUIDs: []int{os.Getuid() + 1}})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		got <- c
	}()
	c, err := Dial(ctx(t), sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("refused caller read: %v, want io.EOF", err)
	}
	ln.Close()
	if c := <-got; c != nil {
		t.Fatal("Accept returned a caller with a wrong uid")
	}
}

func TestListenDirChecks(t *testing.T) {
	base := shortDir(t)

	created := filepath.Join(base, "new", "deeper")
	if _, err := Listen(filepath.Join(created, "k.sock"), Policy{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(created); fi.Mode().Perm() != 0o700 {
		t.Fatalf("created mode %v", fi.Mode().Perm())
	}

	open := filepath.Join(base, "open")
	mustMkdir(t, open, 0o755)
	if _, err := Listen(filepath.Join(open, "k.sock"), Policy{}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("0755 dir: err = %v", err)
	}
	if _, err := Listen(filepath.Join(open, "k.sock"), Policy{DirMode: 0o755}); err != nil {
		t.Fatalf("0755 dir with DirMode 0755: %v", err)
	}

	real := filepath.Join(base, "real")
	mustMkdir(t, real, 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(link, "k.sock"), Policy{}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlinked dir: err = %v", err)
	}

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(file, "k.sock"), Policy{}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("file as dir: err = %v", err)
	}
}

// TestListenForeignDir uses /root, a 0700 dir owned by root, when the test does not run as root.
// TestIPCForeignDir in test/integration covers it in the runner, which always has such a dir.
func TestListenForeignDir(t *testing.T) {
	fi, err := os.Lstat("/root")
	if err != nil || os.Getuid() == 0 || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Skip("needs a 0700 /root and a non-root user")
	}
	_, err = Listen("/root/kwak.sock", Policy{})
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v, want ErrUnsafe", err)
	}
}

func TestDialChecks(t *testing.T) {
	base := shortDir(t)
	dir := filepath.Join(base, "s")
	sock := filepath.Join(dir, "kwak.sock")
	ln, err := Listen(sock, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	if _, err := Dial(ctx(t), sock, []int{os.Getuid() + 1}); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("other uid: err = %v", err)
	}
	if c, err := Dial(ctx(t), sock, []int{os.Getuid() + 1, os.Getuid()}); err != nil {
		t.Fatalf("uid in list: %v", err)
	} else {
		c.Close()
	}

	linkDir := filepath.Join(base, "ld")
	if err := os.Symlink(dir, linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(ctx(t), filepath.Join(linkDir, "kwak.sock"), nil); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlinked dir: err = %v", err)
	}
	linkSock := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, linkSock); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(ctx(t), linkSock, nil); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("symlinked socket: err = %v", err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(ctx(t), plain, nil); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("plain file: err = %v", err)
	}

	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(ctx(t), sock, nil); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("group-writable dir: err = %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := Dial(ctx(t), filepath.Join(dir, "missing.sock"), nil); err == nil {
		t.Fatal("missing socket dialled")
	}
}

func mustMkdir(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(p, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}
