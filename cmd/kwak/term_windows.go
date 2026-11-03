package main

import (
	"encoding/binary"
	"os"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether f is a console (spec 4 step 1), or the pipe of an MSYS or Cygwin
// pty, which is what Git Bash in mintty gives a program. A Bash tool's plain pipe is neither.
func isTerminal(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		return true
	}
	if t, err := windows.GetFileType(h); err != nil || t != windows.FILE_TYPE_PIPE {
		return false
	}
	buf := make([]byte, 4+2*windows.MAX_PATH)
	if windows.GetFileInformationByHandleEx(h, windows.FileNameInfo, &buf[0], uint32(len(buf))) != nil {
		return false
	}
	n := min(int(binary.LittleEndian.Uint32(buf)), len(buf)-4) / 2
	name := make([]uint16, n)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(buf[4+2*i:])
	}
	return isPTYPipeName(string(utf16.Decode(name)))
}
