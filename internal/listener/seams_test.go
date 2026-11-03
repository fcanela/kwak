package listener

import (
	"net"
	"time"

	"github.com/fcanela/kwak/internal/ipc"
	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/internal/proc"
	"github.com/fcanela/kwak/internal/proc/osproc"
	"github.com/fcanela/kwak/internal/secrets"
	"github.com/fcanela/kwak/internal/secrets/oskeyring"
)

// The production implementations P6 wires into Deps. Nothing here runs.
var (
	_ Matrix                                      = (*matrix.Client)(nil)
	_ func(net.Conn) (int, bool, error)           = ipc.PeerPID
	_ func() proc.Reader                          = osproc.New
	_ func(string, time.Duration) secrets.Secrets = oskeyring.New
)
