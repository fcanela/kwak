# Auditing kwak

This file helps you check what the `kwak` binary does before you run it. The design is in
[docs/spec.md](docs/spec.md). Every number and list below was generated from this tree on
2026-11-02 with the command shown next to it, so you can run the same commands and compare.

## Reading order

Read the packages in this order (spec section 2). Each one only depends on the ones before it, or
on small interfaces it declares itself.

1. `internal/wire`: the JSON types of the registrar API, the IPC protocol and `config.json`, and the
   name rules. Data only, no I/O.
2. `internal/paths`: where the config dir, state dir and socket are on each OS.
   `internal/atomicfile` next to it: the one function that replaces a file (temp file, fsync,
   rename, fsync of the directory), used for `config.json`, `state.json`, the Codex files, the
   service files and the registrar's state.
3. `internal/ipc`: the local socket. It checks the socket directory and its owner, reads the
   caller's PID from the kernel, and frames one JSON line per message.
4. `internal/hook`: `kwak hook`. Reads the harness's JSON on stdin, asks the listener over the
   socket, prints what the harness expects.
5. `internal/mcp`: `kwak mcp`. A small JSON-RPC 2.0 server on stdio. It forwards tool calls to the
   listener over the socket.
6. `internal/render`: turns other agents' messages into framed text for a model. Pure.
7. `internal/listener`: the daemon. Sync loop, routing, inboxes, sessions, tools, background resume.
8. `internal/secrets` and `internal/secrets/oskeyring`: the keyring interface, and the only code
   that calls the OS keyring.
9. `internal/service`: the user service (systemd user unit, launchd agent, Windows Run key).

Then the rest of the `kwak` binary:

10. `internal/proc` and `internal/proc/osproc`: process IDs, start times and parents, per OS.
11. `internal/matrix`: the minimal Matrix client-server API client.
12. `internal/regclient`: the HTTPS client of the registrar's `/_kwak/v1` API.
13. `internal/cli`: the human commands (`setup`, `pair`, `unpair`, `status`, `inbox`, `config`) and
    `kwak listen`'s start-up.
14. `internal/setup`: `kwak setup claude|codex` and `kwak doctor`.
15. `cmd/kwak`: the dispatch table. It builds the real implementations and passes them in.

The server side (`cmd/kwak-registrar`, `internal/registrar`) runs on the Matrix host only. Coworkers never
build it.

## Size

Lines per package, non-test and test files, every OS's files included (comments and blank lines
count too):

```sh
for d in $(go list -tags integration -f '{{.Dir}}' ./... | sed "s#^$PWD/##"); do
  src=$(find "$d" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cat {} + | wc -l)
  tst=$(find "$d" -maxdepth 1 -name '*_test.go' -exec cat {} + | wc -l)
  printf '| `%s` | %s | %s |\n' "$d" "$src" "$tst"
done
```

| Package | Non-test lines | Test lines |
|---|---|---|
| `cmd/kwak` | 430 | 511 |
| `cmd/kwak-registrar` | 302 | 271 |
| `deploy` | 0 | 785 |
| `internal/atomicfile` | 43 | 34 |
| `internal/cli` | 1351 | 1436 |
| `internal/hook` | 151 | 360 |
| `internal/ipc` | 505 | 526 |
| `internal/listener` | 3560 | 3597 |
| `internal/matrix` | 489 | 784 |
| `internal/matrix/matrixtest` | 92 | 156 |
| `internal/mcp` | 570 | 777 |
| `internal/paths` | 204 | 234 |
| `internal/proc` | 85 | 136 |
| `internal/proc/osproc` | 208 | 219 |
| `internal/proc/proctest` | 71 | 0 |
| `internal/regclient` | 288 | 529 |
| `internal/registrar` | 3286 | 3286 |
| `internal/render` | 402 | 363 |
| `internal/secrets` | 24 | 0 |
| `internal/secrets/oskeyring` | 125 | 229 |
| `internal/secrets/secretstest` | 76 | 72 |
| `internal/service` | 857 | 923 |
| `internal/service/servicetest` | 160 | 0 |
| `internal/setup` | 1570 | 1044 |
| `internal/wire` | 957 | 885 |
| `internal/wire/wiretest` | 1044 | 570 |
| `test/e2e` | 0 | 4124 |
| `test/integration` | 0 | 1126 |
| `test/integration/ittest` | 695 | 277 |

Non-test lines that end up in each binary (the packages `go list -deps` names for it):

```sh
for c in kwak kwak-registrar; do n=0
  for d in $(go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./cmd/$c | grep "^$PWD"); do
    n=$((n + $(find "$d" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cat {} + | wc -l)))
  done; echo "$c $n"; done
```

- `kwak`: 11,819 lines, over the spec's budget of about 4,000. The listener, the human commands
  and `kwak setup`/`kwak doctor` are most of it.
- `kwak-registrar`: 5,564 lines (`wire`, `proc`, `matrix`, `render`, `atomicfile`, `registrar` and
  its `main`).

## What may open a network connection

Generated with:

```sh
grep -rln --include='*.go' '"net/http"' . | grep -v '_test.go' | sort
grep -rn --include='*.go' -E 'net\.Dial|DialContext|net\.Listen\(|http\.ListenAndServe|\.Serve\(' . | grep -v '_test.go' | sort
grep -rln --include='*.go' '"net"' . | grep -v _test.go | sort
```

Non-test files that import `net/http`:

```
cmd/kwak-registrar/main.go
internal/matrix/client.go
internal/matrix/matrixtest/matrixtest.go
internal/regclient/regclient.go
internal/registrar/agents.go
internal/registrar/api.go
internal/registrar/appservice.go
internal/registrar/pairing.go
internal/registrar/server.go
test/integration/ittest/ittest.go
```

Non-test dials and listens:

```
cmd/kwak-registrar/main.go:113:	asLn, err := net.Listen("tcp", cfg.AppserviceAddr)
cmd/kwak-registrar/main.go:118:	apiLn, err := net.Listen("tcp", cfg.APIAddr)
cmd/kwak-registrar/main.go:135:			if err := servers[i].Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
internal/ipc/ipc.go:150:	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
internal/ipc/ipc.go:210:	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
internal/ipc/ipc.go:60:	ln, err := net.Listen("unix", path)
test/integration/ittest/ittest.go:161:	ln, err := net.Listen("tcp", AppserviceAddr)
test/integration/ittest/ittest.go:166:	go srv.Serve(ln)
test/integration/ittest/ittest.go:252:	c, err := net.DialTimeout("tcp", net.JoinHostPort(addrs[0], "8008"), 3*time.Second)
```

Non-test files that import `net`: `cmd/kwak-registrar/main.go`, `internal/ipc/*.go` (all five OS
files), `internal/listener/{agents,ipcserver,listener}.go`, `internal/mcp/{backend,main}.go`,
`internal/registrar/server.go`, `test/integration/ittest/ittest.go`.

What that means for the `kwak` binary:

| Package | Network use |
|---|---|
| `internal/matrix` | HTTPS client of the Matrix server. It refuses `http://` unless `AllowHTTP` is set, and only the registrar sets it. It never follows a redirect. |
| `internal/regclient` | HTTPS client of the registrar's `/_kwak/v1` API. Refuses `http://` except with a test-only option. |
| `internal/cli` | Builds a `regclient` for `kwak pair`, `kwak setup` and `kwak unpair`, and the one `kwak listen` uses. |
| `internal/listener` | Calls the registrar and the Matrix server only through the `Registrar` and `Matrix` interfaces it is given. It imports `net` for the local socket connections it serves. |
| `cmd/kwak` | Builds the one `http.Client` (`matrix.NewHTTPClient`) for `listen`, `pair`, `setup`, `unpair`, `status`, `inbox` and `config`, and passes it in. `hook`, `mcp`, `doctor` and `version` never build one. |
| `internal/ipc` | Unix domain sockets only (`AF_UNIX`, also on Windows). Nothing leaves the machine. |
| `internal/hook`, `internal/mcp` | Only the local socket, through `ipc`. |

On the server side, `cmd/kwak-registrar` listens on two TCP ports (the appservice port and the
listener API) and `internal/registrar` talks to tuwunel through `matrix`. The test helpers
(`matrixtest`, `ittest`) are never built into a binary.

## What may touch the keyring

```sh
grep -rn --include='*.go' -E '"github.com/zalando/go-keyring"|internal/secrets/oskeyring"' . | sort
```

```
cmd/kwak/main.go:30:	"github.com/fcanela/kwak/internal/secrets/oskeyring"
internal/cli/listen_test.go:20:	"github.com/fcanela/kwak/internal/secrets/oskeyring"
internal/listener/seams_test.go:12:	"github.com/fcanela/kwak/internal/secrets/oskeyring"
internal/secrets/oskeyring/oskeyring.go:10:	"github.com/zalando/go-keyring"
internal/secrets/oskeyring/oskeyring_test.go:11:	"github.com/zalando/go-keyring"
```

- Only `internal/secrets/oskeyring` imports go-keyring. Only `cmd/kwak` builds it, with service
  `kwak` and a 10 s deadline per call. The two test files above only check that its signatures fit;
  they never call it.
- The keyring then reaches three packages, through the `secrets.Secrets` interface:
  - `internal/listener` reads the owner key (`Get`).
  - `internal/cli`: `kwak pair` writes, reads back and deletes a probe item, then writes the owner
    key; `kwak unpair` reads and deletes it.
  - `internal/setup`: `kwak doctor` reads the owner key (`Get`) only when the machine is paired,
    and never prints it.
- `kwak hook`, `kwak mcp` and `kwak version` never build a keyring (tested in
  `cmd/kwak/main_test.go`, `TestAdaptersBuildNoKeyringServiceOrHTTP`).
- The keyring holds exactly one item per machine: service `kwak`, user `<server name>/owner-key`.
- `kwak-registrar` does not import the keyring at all.

## Dependencies

kwak has **two direct dependencies**: `github.com/zalando/go-keyring` (the OS keyring without cgo)
and `golang.org/x/sys` (terminal checks, peer credentials, process info, the Windows registry).
go-keyring brings in two more that are compiled into `kwak`: `github.com/godbus/dbus/v5` (Linux,
to talk to the Secret Service) and `github.com/danieljoos/wincred` (Windows Credential Manager).
Everything else is the Go standard library.

What is linked into each binary, per OS:

```sh
for os in linux darwin windows; do for c in kwak kwak-registrar; do
  echo "$os $c: $(GOOS=$os CGO_ENABLED=0 go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' ./cmd/$c | sort -u | tr '\n' ' ')"
done; done
```

```
linux kwak: github.com/godbus/dbus/v5 github.com/zalando/go-keyring golang.org/x/sys
linux kwak-registrar:
darwin kwak: github.com/zalando/go-keyring golang.org/x/sys
darwin kwak-registrar:
windows kwak: github.com/danieljoos/wincred github.com/zalando/go-keyring golang.org/x/sys
windows kwak-registrar:
```

The full module graph also lists `testify` and its own dependencies. They are only there because
wincred's tests use them (`go mod why -m github.com/stretchr/testify` shows the path through
`github.com/danieljoos/wincred.test`). They are never compiled into kwak.

`go list -m all`:

```
github.com/fcanela/kwak
github.com/danieljoos/wincred v1.2.3
github.com/davecgh/go-spew v1.1.1
github.com/godbus/dbus/v5 v5.2.2
github.com/pmezard/go-difflib v1.0.0
github.com/stretchr/objx v0.5.2
github.com/stretchr/testify v1.11.1
github.com/zalando/go-keyring v0.2.8
golang.org/x/sys v0.49.0
gopkg.in/yaml.v3 v3.0.1
```

`go mod graph`:

```
github.com/fcanela/kwak github.com/danieljoos/wincred@v1.2.3
github.com/fcanela/kwak github.com/godbus/dbus/v5@v5.2.2
github.com/fcanela/kwak github.com/zalando/go-keyring@v0.2.8
github.com/fcanela/kwak go@1.26.0
github.com/fcanela/kwak golang.org/x/sys@v0.49.0
github.com/danieljoos/wincred@v1.2.3 github.com/stretchr/testify@v1.11.1
github.com/danieljoos/wincred@v1.2.3 golang.org/x/sys@v0.20.0
github.com/danieljoos/wincred@v1.2.3 github.com/davecgh/go-spew@v1.1.1
github.com/danieljoos/wincred@v1.2.3 github.com/pmezard/go-difflib@v1.0.0
github.com/danieljoos/wincred@v1.2.3 github.com/stretchr/objx@v0.5.2
github.com/danieljoos/wincred@v1.2.3 gopkg.in/yaml.v3@v3.0.1
github.com/godbus/dbus/v5@v5.2.2 golang.org/x/sys@v0.27.0
github.com/zalando/go-keyring@v0.2.8 github.com/danieljoos/wincred@v1.2.3
github.com/zalando/go-keyring@v0.2.8 github.com/godbus/dbus/v5@v5.2.2
github.com/zalando/go-keyring@v0.2.8 golang.org/x/sys@v0.27.0
go@1.26.0 toolchain@go1.26.0
golang.org/x/sys@v0.49.0 go@1.26.0
```

`go mod verify` says "all modules verified".

### Licences

Read from the `LICENSE` file of each module in the module cache
(`go list -m -f '{{.Dir}}' all`, then `$dir/LICENSE`):

| Module | Version | In the kwak binary | Licence |
|---|---|---|---|
| `github.com/zalando/go-keyring` | v0.2.8 | yes, every OS | MIT (Zalando SE) |
| `golang.org/x/sys` | v0.49.0 | yes, every OS | BSD 3-Clause (The Go Authors) |
| `github.com/godbus/dbus/v5` | v5.2.2 | Linux only | BSD 2-Clause (Georg Reinke, Google) |
| `github.com/danieljoos/wincred` | v1.2.3 | Windows only | MIT (Daniel Joos) |
| `github.com/stretchr/testify` | v1.11.1 | no, wincred's tests | MIT |
| `github.com/stretchr/objx` | v0.5.2 | no, wincred's tests | MIT |
| `github.com/davecgh/go-spew` | v1.1.1 | no, wincred's tests | ISC |
| `github.com/pmezard/go-difflib` | v1.0.0 | no, wincred's tests | BSD 3-Clause |
| `gopkg.in/yaml.v3` | v3.0.1 | no, wincred's tests | MIT and Apache 2.0 (see its LICENSE and NOTICE) |

kwak itself is MIT (`LICENSE` at the repo root). The repo is private for now (spec section 12,
Q13).

## Checks you can run

```sh
go build ./...
go vet ./...
go test -race ./...
make check          # go vet and go build for linux, darwin and windows on amd64 and arm64
go mod verify
```

The integration and end-to-end tests need docker and local images; see the `Makefile`.
