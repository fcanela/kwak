//go:build !linux && !darwin && !windows

package osproc

import (
	"errors"

	"github.com/fcanela/kwak/internal/proc"
)

type reader struct{}

// New returns a Reader that knows no process on this OS.
func New() proc.Reader { return reader{} }

func (reader) Lookup(int) (proc.ID, proc.ID, error) {
	return proc.ID{}, proc.ID{}, errors.ErrUnsupported
}
