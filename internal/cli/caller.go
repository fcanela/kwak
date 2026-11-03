package cli

import (
	"context"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
)

// SocketCaller returns a Caller that dials the listener's socket at path (ipc.Dial with its owner
// checks), sends one line and reads one line, within 30 s when ctx has no deadline.
func SocketCaller(path string) Caller {
	return func(ctx context.Context, req, reply any) error {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
		}
		return ipc.Call(ctx, path, 0, req, reply)
	}
}
