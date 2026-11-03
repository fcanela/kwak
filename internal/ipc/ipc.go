// Package ipc is the listener's local socket: listen and dial with owner checks, an exclusive
// lock, newline-delimited JSON framing and peer credentials per OS.
package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// MaxLine is the largest line, newline excluded.
const MaxLine = 1 << 20

// ErrLineTooLong is returned by LineReader.Next and WriteLine for a line over MaxLine.
var ErrLineTooLong = errors.New("ipc: line longer than 1 MiB")

var (
	// ErrUnsafe wraps every refusal of a socket path, its directory or its peer.
	ErrUnsafe = errors.New("ipc: unsafe socket")
	// ErrPathTooLong is returned by Listen for a path that does not fit in a sockaddr_un.
	ErrPathTooLong = errors.New("ipc: socket path too long")
	// ErrLocked is returned by Lock when another holder has the lock.
	ErrLocked = errors.New("ipc: already locked")
	// ErrInUse is returned by RemoveStale when a listener answers on the socket.
	ErrInUse = errors.New("ipc: a listener answers on the socket")
)

// Policy says who may connect and how the socket directory must look.
type Policy struct {
	AllowedUIDs []int       // callers the listener accepts; default: its own uid
	DirMode     os.FileMode // default 0700; hardened mode would use a group (7.5)
}

// Listen checks the socket directory (5.6), creating it when missing, checks the path length,
// and listens on a unix socket at path. It removes nothing and holds no lock: the caller holds
// listen.lock and calls RemoveStale first. Accept drops callers whose uid is not in
// p.AllowedUIDs, so the returned connections need no second uid check. On Windows the uid
// and mode checks are skipped and the directory ACL decides.
func Listen(path string, p Policy) (net.Listener, error) {
	mode := p.DirMode
	if mode == 0 {
		mode = 0o700
	}
	if len(path) > maxPath {
		return nil, fmt.Errorf("%w: %d bytes, the limit is %d", ErrPathTooLong, len(path), maxPath)
	}
	if err := prepareDir(filepath.Dir(path), mode); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	allowed := p.AllowedUIDs
	if len(allowed) == 0 {
		allowed = []int{os.Getuid()}
	}
	return &filtered{Listener: ln, allowed: allowed}, nil
}

func prepareDir(dir string, mode os.FileMode) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return err
		}
		switch err := os.Mkdir(dir, mode); {
		case err == nil:
			if err := os.Chmod(dir, mode); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrExist):
			return err
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if err := realDir(dir, fi); err != nil {
		return err
	}
	uid, ok := owner(fi)
	if !ok {
		return nil
	}
	if me := os.Getuid(); uid != me {
		return fmt.Errorf("%w: %s is owned by uid %d, not %d", ErrUnsafe, dir, uid, me)
	}
	if perm := fi.Mode().Perm(); perm != mode {
		return fmt.Errorf("%w: %s has mode %04o, want %04o", ErrUnsafe, dir, perm, mode)
	}
	return nil
}

func realDir(dir string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrUnsafe, dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafe, dir)
	}
	return nil
}

type filtered struct {
	net.Listener
	allowed []int
}

func (l *filtered) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, ok, err := PeerUID(c)
		if err == nil && (!ok || slices.Contains(l.allowed, uid)) {
			return c, nil
		}
		c.Close()
	}
}

// RemoveStale removes a socket file at path that no listener answers. Only the holder of
// listen.lock calls it, before Listen. A missing file is not an error.
func RemoveStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkSocket(path, fi, []int{os.Getuid()}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		c.Close()
		return ErrInUse
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("ipc: %s does not answer: %w", path, err)
	}
	return os.Remove(path)
}

func checkSocket(path string, fi os.FileInfo, uids []int) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrUnsafe, path)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket", ErrUnsafe, path)
	}
	uid, ok := owner(fi)
	if !ok {
		return nil
	}
	if !slices.Contains(uids, uid) {
		return fmt.Errorf("%w: %s is owned by uid %d, not %v", ErrUnsafe, path, uid, uids)
	}
	return nil
}

// Dial checks that the directory and the socket are owned by one of serverUIDs (default: its own
// uid), that neither is a symlink and that the directory is not group or world writable, then
// connects and checks the peer uid again. On Windows only the symlink and socket type checks
// apply.
func Dial(ctx context.Context, path string, serverUIDs []int) (net.Conn, error) {
	uids := serverUIDs
	if len(uids) == 0 {
		uids = []int{os.Getuid()}
	}
	dir := filepath.Dir(path)
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if err := realDir(dir, fi); err != nil {
		return nil, err
	}
	if uid, ok := owner(fi); ok {
		if !slices.Contains(uids, uid) {
			return nil, fmt.Errorf("%w: %s is owned by uid %d, not %v", ErrUnsafe, dir, uid, uids)
		}
		if perm := fi.Mode().Perm(); perm&0o022 != 0 {
			return nil, fmt.Errorf("%w: %s has mode %04o, writable by others", ErrUnsafe, dir, perm)
		}
	}
	if fi, err = os.Lstat(path); err != nil {
		return nil, err
	}
	if err := checkSocket(path, fi, uids); err != nil {
		return nil, err
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	uid, ok, err := PeerUID(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	if ok && !slices.Contains(uids, uid) {
		c.Close()
		return nil, fmt.Errorf("%w: the listener runs as uid %d, not %v", ErrUnsafe, uid, uids)
	}
	return c, nil
}

// LineReader reads newline-delimited lines of at most MaxLine bytes.
type LineReader struct {
	r   *bufio.Reader
	err error
}

// NewLineReader wraps r.
func NewLineReader(r io.Reader) *LineReader {
	return &LineReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next line without its newline, io.EOF at the end, or ErrLineTooLong. A last
// line without a newline is returned before io.EOF. After any error, Next returns it again.
// The returned slice is the caller's.
func (l *LineReader) Next() ([]byte, error) {
	if l.err != nil {
		return nil, l.err
	}
	var line []byte
	for {
		frag, err := l.r.ReadSlice('\n')
		line = append(line, frag...)
		n := len(line)
		if err == nil {
			n--
		}
		if n > MaxLine {
			l.err = ErrLineTooLong
			return nil, l.err
		}
		switch {
		case err == nil:
			return line[:n], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && n > 0:
			l.err = io.EOF
			return line, nil
		default:
			l.err = err
			return nil, err
		}
	}
}

// WriteLine writes v as one JSON line in a single Write, without HTML escaping.
func WriteLine(w io.Writer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	if b.Len()-1 > MaxLine {
		return ErrLineTooLong
	}
	_, err := w.Write(b.Bytes())
	return err
}

// ErrDial wraps Call's failure to connect to the socket.
var ErrDial = errors.New("ipc: cannot connect")

// Call sends req as one line to the socket at path (Dial's checks, its own uid) and decodes the one
// line it gets back into reply. dialTimeout > 0 bounds the connect; ctx's deadline bounds the whole
// exchange.
func Call(ctx context.Context, path string, dialTimeout time.Duration, req, reply any) error {
	dctx := ctx
	if dialTimeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	c, err := Dial(dctx, path, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDial, err)
	}
	defer c.Close()
	if d, ok := ctx.Deadline(); ok {
		c.SetDeadline(d)
	}
	if err := WriteLine(c, req); err != nil {
		return err
	}
	line, err := NewLineReader(c).Next()
	if err != nil {
		return err
	}
	return json.Unmarshal(line, reply)
}
