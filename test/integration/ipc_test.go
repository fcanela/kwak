//go:build integration && linux

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/test/integration/ittest"
)

// TestIPCForeignDir checks spec 10.2 (ipc): Listen and Dial refuse a 0700 dir of another uid.
func TestIPCForeignDir(t *testing.T) {
	env := ittest.Start(t)
	if env == nil {
		return
	}
	fi, err := os.Lstat(ittest.ForeignDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("foreign dir: %v, %v", fi, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != ittest.ForeignUID || os.Getuid() == ittest.ForeignUID {
		t.Fatalf("foreign dir owner %+v, runner uid %d", fi.Sys(), os.Getuid())
	}
	sock := filepath.Join(ittest.ForeignDir, "kwak.sock")
	if ln, err := ipc.Listen(sock, ipc.Policy{}); !errors.Is(err, ipc.ErrUnsafe) {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("Listen in a dir of uid %d: err = %v, want ErrUnsafe", ittest.ForeignUID, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if c, err := ipc.Dial(ctx, sock, nil); !errors.Is(err, ipc.ErrUnsafe) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("Dial in a dir of uid %d: err = %v, want ErrUnsafe", ittest.ForeignUID, err)
	}
}
