package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/paths"
	"github.com/fcanela/kwak/internal/wire"
)

// ShutdownAt sends the shutdown op to the listener at socket path and waits for its answer.
// ErrNotRunning wraps any failure to connect.
func ShutdownAt(ctx context.Context, path string) error {
	var a wire.Ack
	err := ipc.Call(ctx, path, 0, wire.Head{V: wire.ProtocolVersion, Op: wire.OpShutdown}, &a)
	switch {
	case errors.Is(err, ipc.ErrDial):
		return fmt.Errorf("%w: %w", ErrNotRunning, err)
	case err != nil:
		return fmt.Errorf("no answer to shutdown: %w", err)
	case !a.OK:
		return fmt.Errorf("the listener refused to shut down: %s", a.Err)
	}
	return nil
}

// Shutdown is ShutdownAt on paths.SocketPath with a 5 s limit.
func Shutdown() error {
	p, err := paths.SocketPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ShutdownAt(ctx, p)
}
