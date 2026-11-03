//go:build integration && linux

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIdleCost measures one paired listener with no sessions: RSS, CPU and Matrix requests over a
// minute of idle. It only logs; it has no thresholds.
//
// The shipped kwak binary cannot run kwak listen in the runner: the image has no session bus, so
// the keyring preflight stops it. The test logs that refusal, then measures the same listener code
// in its own process (E22's child), with the real Matrix and registrar clients over the edge and an
// in-memory keyring. That process is the test binary, so its RSS is an upper bound.
func TestIdleCost(t *testing.T) {
	if os.Getenv("KWAK_IT_INNER") == "" && os.Getenv("KWAK_IT_IDLE") == "" {
		t.Skip("set KWAK_IT_IDLE=1 to measure idle cost (about 75 s)")
	}
	w := Start(t)
	if w == nil {
		return
	}
	idleShippedBinary(t, w)

	w.Alice.StopListener(t)
	w.Bob.StopListener(t)
	ch := e2xStartChild(t, w, w.Alice, e2xEdgeCert(t, w))
	w.Alice.WaitSyncing(t)
	pid := ch.cmd.Process.Pid

	time.Sleep(10 * time.Second)
	const window = 60 * time.Second
	t0 := time.Now()
	cpu0 := idleCPUTicks(t, pid)
	rss0 := idleRSS(t, pid)
	rssMax := rss0
	for time.Since(t0) < window {
		time.Sleep(5 * time.Second)
		rssMax = max(rssMax, idleRSS(t, pid))
	}
	cpu1 := idleCPUTicks(t, pid)
	t1 := time.Now()
	rss1 := idleRSS(t, pid)
	secs := t1.Sub(t0).Seconds()

	var all, syncs int
	tokens := map[string]bool{}
	var gaps []string
	var last time.Time
	for _, r := range w.Requests(t0) {
		if r.At.After(t1) {
			continue
		}
		all++
		if strings.HasSuffix(r.Path, "/sync") {
			syncs++
			tokens[r.Token] = true
			if !last.IsZero() {
				gaps = append(gaps, r.At.Sub(last).Round(100*time.Millisecond).String())
			}
			last = r.At
		}
	}
	perMin := func(n float64) float64 { return n * 60 / secs }
	// USER_HZ is 100 on Linux, so one tick is 10 ms.
	t.Logf("IDLE rss_kb=%d rss_start_kb=%d rss_max_kb=%d cpu_ms_per_min=%.1f requests_per_min=%.1f sync_per_min=%.1f sync_tokens=%d window_s=%.1f",
		rss1, rss0, rssMax, perMin(float64(cpu1-cpu0)*10), perMin(float64(all)), perMin(float64(syncs)), len(tokens), secs)
	t.Logf("IDLE sync gaps %v", gaps)
	ch.stop(t)
}

// idleShippedBinary runs the shipped kwak listen with fresh dirs and logs how it ends.
func idleShippedBinary(t *testing.T, w *World) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(w.Kwak, "listen")
	cmd.Env = append(withEnv(withEnv(os.Environ(), envFake, ""), "DBUS_SESSION_BUS_ADDRESS", ""),
		"KWAK_CONFIG_DIR="+filepath.Join(dir, "config"), "KWAK_STATE_DIR="+filepath.Join(dir, "state"),
		"KWAK_SOCKET="+filepath.Join(dir, "kwak.sock"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kwak listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Logf("IDLE shipped kwak listen exited %d: %s", exitCode(err), strings.TrimSpace(out.String()))
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Logf("IDLE shipped kwak listen still ran after 10 s (killed): %s", strings.TrimSpace(out.String()))
	}
}

func idleRSS(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			if err != nil {
				t.Fatalf("VmRSS %q: %v", v, err)
			}
			return n
		}
	}
	t.Fatalf("no VmRSS in /proc/%d/status", pid)
	return 0
}

// idleCPUTicks is utime plus stime from /proc/<pid>/stat, fields 14 and 15.
func idleCPUTicks(t *testing.T, pid int) int64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 13 {
		t.Fatalf("short /proc/%d/stat: %q", pid, s)
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	k, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("utime/stime in %q", s)
	}
	return u + k
}
