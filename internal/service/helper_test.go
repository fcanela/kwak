package service_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as the child process of the supervisor and detach tests.
const helperEnv = "KWAK_SERVICE_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "runs":
		// Exits 1 on its first run and 0 on its second, counting runs in a file.
		f := os.Getenv("KWAK_SERVICE_COUNT")
		b, _ := os.ReadFile(f)
		n := len(b) + 1
		_ = os.WriteFile(f, []byte(strings.Repeat("x", n)), 0o600)
		fmt.Fprintf(os.Stderr, "child run %d\n", n)
		if n == 1 {
			os.Exit(1)
		}
		os.Exit(0)
	case "detach":
		out := os.Getenv("KWAK_SERVICE_OUT")
		_ = os.WriteFile(out+".tmp", []byte(strings.Join(os.Args[1:], " ")+"\n"+strconv.Itoa(os.Getpid())+"\n"+sessionLeader()), 0o600)
		_ = os.Rename(out+".tmp", out)
		os.Exit(0)
	}
	os.Exit(3)
}
