package main

import "strings"

// isPTYPipeName matches the pipe names MSYS and Cygwin give a pty:
// \{msys,cygwin}-<hex>-pty<N>-{from,to}-master.
func isPTYPipeName(name string) bool {
	t := strings.Split(name, "-")
	if len(t) < 5 {
		return false
	}
	switch strings.TrimPrefix(t[0], `\Device\NamedPipe`) {
	case `\msys`, `\cygwin`:
	default:
		return false
	}
	return t[1] != "" && strings.HasPrefix(t[2], "pty") && (t[3] == "from" || t[3] == "to") && t[4] == "master"
}
