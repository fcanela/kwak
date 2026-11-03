//go:build !windows

package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func currentGOOS() string { return runtime.GOOS }

func TestOwnedDir(t *testing.T) {
	dir := t.TempDir()
	uid := os.Getuid()
	if !ownedDir(dir, uid) {
		t.Fatal("own temp dir not owned")
	}
	if ownedDir(dir, uid+1) {
		t.Fatal("owned by another uid")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if ownedDir(link, uid) {
		t.Fatal("symlink accepted")
	}
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ownedDir(f, uid) || ownedDir(filepath.Join(dir, "missing"), uid) {
		t.Fatal("file or missing path accepted")
	}
}
