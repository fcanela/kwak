//go:build unix

package service_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/service"
)

func TestDetach(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv(helperEnv, "detach")
	t.Setenv("KWAK_SERVICE_OUT", out)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Detach(exe); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var b []byte
	for {
		if b, err = os.ReadFile(out); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached child never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) != 3 || lines[0] != "listen --supervise" || lines[2] != "true" {
		t.Fatalf("child wrote %q", b)
	}
	// The child was released, not waited on; reap it so no zombie stays behind.
	if pid, err := strconv.Atoi(lines[1]); err == nil {
		var ws syscall.WaitStatus
		for i := 0; i < 500; i++ {
			if p, _ := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); p == pid {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
