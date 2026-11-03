//go:build integration

package deploy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Images are never pulled: a test whose image is missing locally skips and names it.
const nginxImage = "nginx:1.30.6-alpine"

func docker(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", args...)
}

func needImage(t *testing.T, ctx context.Context, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := docker(ctx, "image", "inspect", image).Run(); err != nil {
		t.Skipf("image %s is not present locally; pull it by hand to run this test", image)
	}
}

func suffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func TestDockerBuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	from := regexp.MustCompile(`(?m)^FROM (\S+) AS build$`).FindStringSubmatch(readText(t, "Dockerfile"))
	if from == nil {
		t.Fatal("no build stage in the Dockerfile")
	}
	needImage(t, ctx, from[1])
	// KWAK_IT_BUILD_ROOT points the build at another checkout, for example one with a stub registrar.
	root, err := filepath.Abs(cmp.Or(os.Getenv("KWAK_IT_BUILD_ROOT"), ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "kwak-registrar", "main.go")); err != nil {
		t.Skip("cmd/kwak-registrar does not exist yet")
	}
	tag := "kwak-it-registrar-" + suffix(t) + ":test"
	build := docker(ctx, "build", "--pull=false", "--network=none", "--label", "kwak-it=1",
		"-f", filepath.Join(root, "deploy", "registrar", "Dockerfile"), "-t", tag, root)
	build.Stdout, build.Stderr = t.Output(), t.Output()
	if err := build.Run(); err != nil {
		t.Fatalf("docker build: %v", err)
	}
	t.Cleanup(func() { _ = docker(context.Background(), "rmi", "-f", tag).Run() })

	// The tokens it prints stay in memory and are never logged.
	run := docker(ctx, "run", "--rm", "--pull", "never", "--network", "none", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--label", "kwak-it=1",
		tag, "gen-registration", "-url", "http://kwak-registrar:8009", "-server", "kwak.test", "-o", "-")
	var stderr bytes.Buffer
	run.Stderr = &stderr
	out, err := run.Output()
	if err != nil {
		t.Fatalf("gen-registration in the image: %v\n%s", err, stderr.String())
	}
	if err := checkGenerated(out, "http://kwak-registrar:8009", "kwak.test"); err != nil {
		t.Errorf("the image's registration: %v", err)
	}
}

// TestNginxLocationsParse runs `nginx -t` on the two locations inside a server shaped like a Matrix
// site: the same matrix_auth zone, the same server-level lines.
func TestNginxLocationsParse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	needImage(t, ctx, nginxImage)
	locs := strings.ReplaceAll(readText(t, "nginx-locations.example.conf"),
		"192.0.2.12:8010", "127.0.0.1:8010")
	conf := `events {}
http {
  map $request_method $matrix_auth_key { POST $binary_remote_addr; default ""; }
  limit_req_zone $matrix_auth_key zone=matrix_auth:10m rate=30r/m;
  server {
    listen 8443;
    server_name kwak.test;
    default_type application/json;
    client_max_body_size 50m;
` + locs + `
    location / { return 404; }
  }
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := docker(ctx, "run", "--rm", "--pull", "never", "--network", "none", "--label", "kwak-it=1",
		"--name", "kwak-it-nginx-"+suffix(t),
		"-v", dir+":/kwak:ro", nginxImage, "nginx", "-t", "-c", "/kwak/nginx.conf")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nginx -t: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "test is successful") {
		t.Errorf("nginx -t said:\n%s", out)
	}
}
