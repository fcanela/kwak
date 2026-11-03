# kwak: build plan

This plan splits the build described in [spec.md](spec.md) into work packages. Package 0 is done
first, by one agent. Packages 1 to 4 then run in parallel worktrees, each owning disjoint paths and
building only against package 0's types and interfaces. Package 5 (deployment files) can run at the
same time. Package 6 wires everything and runs the end-to-end test.

```
P0 foundation ──gate──┬── P1 registrar ─────────────┐
                      ├── P2 listener + cli ────────┤
                      ├── P3 harness adapters ──────┼── P6 integration (wiring + e2e)
                      ├── P4 platform (OS code) ────┤
                      └── P5 deployment artefacts ──┘
```

## Rules for every package

- Read `docs/spec.md` first. It is the source of truth; this plan only says who builds what.
- Tests always. `go test -race ./...` must pass for the packages you own. Doubles only through the
  interfaces of spec section 2.1, passed to constructors. No monkeypatching, no swapped package
  variables, no mocking libraries. Comments rare and terse.
- Standard library only, except `github.com/zalando/go-keyring` (only in `internal/secrets/oskeyring`)
  and `golang.org/x/sys`. Any other dependency needs a written reason and the maintainer's yes.
- Do not change files you do not own. If a package 0 type or interface is wrong, stop and report it
  to the orchestrator: package 0's owner changes it, then every package rebases.
- Do not add files under `cmd/kwak/`. Each package exposes `Main(args []string, stdin io.Reader,
  stdout, stderr io.Writer) int` (where it has a command), or a constructor that takes the OS
  implementations it needs and returns such a `Main` (spec 2.1), and P6 wires it into the dispatch
  table.
- Never name the real server in tests. Never touch `~/.claude`, `~/.codex`, the Matrix host or the proxy. Never run
  a billable harness without the maintainer's go-ahead.
- Plain English in docs and messages: short sentences, no em-dashes.
- `make check` (vet and build for all six GOOS/GOARCH pairs) must stay green.

---

## P0: foundation (one agent, first)

**Goal:** everything the parallel packages compile and test against, plus the docker harness and the
fact probes that measure what the spec only read.

**Owns:**

- `go.mod`, `go.sum`, `Makefile`, `.gitignore`
- `cmd/kwak/main.go` (skeleton: `kwak version` and an empty dispatch table that P6 fills)
- `internal/wire/**`: registrar API request and response structs (spec 3.4) and the errcode
  constants (`wire.ErrBadKey`, `wire.ErrNotYours`, `wire.ErrParentClosed`, `wire.ErrOwnerRemoved`,
  `wire.ErrBadSession`, `wire.ErrNamesExhausted`, `wire.ErrHomeserver`, `wire.ErrExpired`); IPC
  messages (`HookRequest`, `HookResponse`, `MCPHello`, `ToolRequest`, `ToolResponse`,
  `StatusResponse`, `ReloadResponse`, spec 5.6); name rules (`ValidSessionID`, `SessionHex`,
  `Candidates`, `HelperLocalpart`, `StandingLocalpart`, `ParseLocalpart`, `OwnerName`, `HelperName`,
  spec 3.6); the registration file type and `GenRegistration(url, serverName string, rnd io.Reader)
  ([]byte, error)` (spec 3.7)
- `internal/wire/wiretest/**`: the two **contract tables** every side tests against, so no package
  tests a contract only from its own reading:
  - the IPC golden table: request and response lines for each harness and hook event (including
    Claude `PostToolUseFailure` and stdin with unknown fields), `mcp_hello`, `tool` per tool,
    `status`, `inbox`, `reload`, `shutdown`, the version mismatch, and the error replies;
  - the HTTP transcript table: for every `/_kwak/v1` route, method, path, auth, request body, status
    and errcode, for the success case and each error of spec 3.4 and the 5.1 errcode table (201
    `/pair`, 202, 200 and 410 on `/pair/claim`, 403 `KWAK_NOT_YOURS` against `KWAK_PARENT_CLOSED`, 400
    `KWAK_BAD_SESSION`, 409, 502 `KWAK_HOMESERVER`, 401, 429).
- `internal/matrix/**`: the client of spec 2.1, with `ForwardedFor`, `AsUser`, `AsDevice`, limits and
  error parsing; a small `matrixtest` helper that registers humans with a token
- `internal/paths/**`: `ConfigDir`, `StateDir`, `SocketPath` per OS, with the `KWAK_SOCKET`,
  `KWAK_STATE_DIR` and `KWAK_CONFIG_DIR` test overrides (spec 5.6)
- `internal/ipc/**`: `Policy`, `Listen`, `Dial` (with the owner checks of spec 5.6), `Lock`, line
  framing, `PeerPID`, `PeerUID` per OS
- `internal/proc/proc.go` and `internal/proc/proctest/**`: `ID`, `Reader`, `Chain`, `Alive`, and a
  scripted `Reader` double (no OS code)
- `internal/secrets/secrets.go` and `internal/secrets/secretstest/**`: `Secrets`, `ErrNotFound`, `Mem`
- `test/integration/**`: the docker harness and the fact probes (spec 10.3)
- `test/cross.sh`

**Provides:** all of the above, as the shared contract.

**Docker harness** (`test/integration`, build tag `integration`):

- A Go helper package `ittest` with `Start(t) *Env`. It creates `docker network create --internal
  kwak-it-<rand>`, writes the tuwunel config from `testdata/tuwunel.toml.tmpl` (server `kwak.test`,
  federation off, registration by token, `ip_source = "rightmost_x_forwarded_for"`,
  `ip_source_trusted_subnets = []`, `error_on_unknown_config_opts = true`,
  `appservice_dir = "/run/secrets/appservices"`), writes the registration with
  `wire.GenRegistration("http://kwak-it-runner:8009", "kwak.test", ...)`, and starts
  `ghcr.io/matrix-construct/tuwunel:v1.9.4` (local image, never pulled) read-only, `--cap-drop ALL`,
  `no-new-privileges`, `--user 65534:65534`, tmpfs for `/tmp` and `/var/lib/tuwunel`.
- It builds a `FROM scratch` runner image from host binaries built with `CGO_ENABLED=0`: the test
  binary of the **calling** package (`Start` copies `os.Executable()` when `KWAK_IT_INNER` is unset,
  so P1's `./internal/registrar/...` and P6's `./test/e2e` run their own tests inside), plus extra
  binaries a caller asks for. The Makefile's integration targets force `CGO_ENABLED=0`, and `Start`
  fails with a clear message when `debug.ReadBuildInfo` does not show `CGO_ENABLED=0`. It runs the
  image with alias `kwak-it-runner`, `HOME=/tmp/home` and `KWAK_IT_INNER=1`. Inside, `Env` exposes the homeserver URL, the
  registration tokens, a helper to register humans (always with `X-Forwarded-For`), and a way to serve
  an appservice handler on `:8009`.
- No `--privileged`, no `--network host`, no published ports. The harness refuses to start if any
  generated file or environment value contains the real server name. `t.Cleanup` removes containers and
  the network, also on failure.
- P6 reuses `ittest` for the e2e scenarios.

**Acceptance tests that must pass:**

- `go test -race ./internal/...` (wire tables, matrix against `httptest.Server`, paths, ipc with real
  sockets, proc logic with the double).
- `go test -tags integration ./test/integration` runs fact probes F1 to F13 (spec 10.3) and records
  each measured answer in `test/integration/FACTS.md`.
- `wiretest` tables parse and re-encode unchanged, and every errcode constant appears in the HTTP
  table.
- `make check` green for linux, darwin and windows on amd64 and arm64.

**The gate.** P1 to P4 start only after the probes ran. If a probe contradicts the spec, P0 applies
the fallback named in spec 10.3 to `docs/spec.md` (and to `internal/wire` or `internal/matrix` when the
fallback touches them) before the gate opens, and writes what changed at the top of `FACTS.md`.
**Except F4, F5 and F6:** their fallbacks change the security posture (F4 lets any deactivated local
account join the room, F5 puts a password on the standing account, F6 makes one machine's revoke log
out all of an owner's machines). If one of them fails, the gate stays closed and P0 reports the
measured answer and the candidate fallback to the maintainer, who decides.

**Must not touch:** anything outside the paths above.

---

## P1: registrar

**Owns:** `internal/registrar/**`, `cmd/kwak-registrar/**`.

**Consumes:** `wire` (API types, names, registration), `matrix.Client`, the `ittest` harness.

**Provides:**

- `registrar.Server` with `API()`, `Appservice()`, `RunJobs()` (spec 2.1, 3.1 to 3.11).
- `registrar.Store`, the JSON state file (spec 3.2).
- The `Homeserver` adapter over `matrix.Client` with the `as_token` and `ForwardedFor: "127.0.0.1"`.
- `cmd/kwak-registrar`: `serve -config`, `gen-registration -url -server [-format json|yaml]`,
  `create-room -homeserver` (token on stdin, never argv).

**Acceptance tests that must pass:**

- `go test -race ./internal/registrar/... ./cmd/kwak-registrar/...` with a `fakeHomeserver` double,
  a fixed `Clock` and a fixed `Rand`, covering spec 10.2 "registrar" and "registrar.Store": pairing
  (start, bind by DM, claim once, expiry, wrong code, unconfirmed revoke), agents (new, rotate, reopen,
  clash ladder, `M_USER_IN_USE`, crash-safe `pending`, `request_id` replay, `KWAK_NOT_YOURS`, re-pair
  `replaces`), close, idle close, key warn and expiry, revoke (I9), owner removal, transaction auth and
  dedupe, `done_events`, the `FetchEvent` re-check for every state-changing command, commands only
  in the recorded DM, encrypted-room invite handling, DM leave and `dm reset`, owner name from the
  localpart, re-pair confirmation moving records and revoking the old pairing, new-IP notices,
  display-name reset, the per-session mutex under concurrent requests, XFF on every outbound request.
- The API handler passes every row of the `wiretest` HTTP transcript table.
- `go test -tags integration ./internal/registrar/...` (or a test under `internal/registrar/it`):
  the real `Server` against tuwunel through `ittest`, for pairing through a real DM and one agent
  create, close and reopen.

**Must not touch:** `internal/listener`, `internal/hook`, `internal/mcp`, any `cmd/kwak` file, any
package 0 path.

---

## P2: listener daemon, render, registrar client, human commands

**Owns:** `internal/listener/**`, `internal/render/**`, `internal/regclient/**`, `internal/cli/**`.

**Consumes:** `wire`, `matrix.Client`, `ipc`, `paths`, `proc` (interface and `proctest`), `secrets`
(interface and `secretstest`).

**Provides:**

- `listener.New(cfg, stateDir, deps)` and `Run(ctx, ln)` (spec 2.1, section 5): sync loop with gap
  fill, `Route`, inboxes with atomic drains, held messages, session linking from peer PID and chains,
  session lifecycle and agent creation, close grace, alive pings, question timeout, helpers,
  volunteering, background resume through `Runner`, the IPC server for every op in spec 5.6, and the
  tool handlers (`send`, `reply`, `thread`, `helper`, `volunteer`, `status`).
- `render.Delivery` and `render.Sanitize` (spec 6.1).
- `regclient`: the HTTPS client of `/_kwak/v1` (implements `listener.Registrar`, plus `Pair` and
  `Claim` for `kwak pair`).
- `cli`: `cli.New(env cli.Env)` returning the `Main` functions for bare `setup`, `pair`, `unpair`,
  `status`, `inbox`, `config` (spec 4, 5.13), and `cli.Listen(cfg, listener.Deps)` for the daemon.
  `cli.Env` carries the OS implementations as interfaces: `Secrets`, `Service` (`service.Manager`),
  `Preflight func() error`, `Call` (the IPC caller), `PrintHarness func(io.Writer, wire.Harness)
  error` and `IsTerminal func() bool`. P6 builds them. Bare `kwak setup` (pair, install, wait for the
  sync, print harness steps) is P2's, tested with doubles for all of them. `listen` flag parsing,
  including the Windows `--detach` and `--supervise` dispatch, is in P6's `main.go`.

**Acceptance tests that must pass:**

- `go test -race ./internal/listener/... ./internal/render/... ./internal/regclient/... ./internal/cli/...`
  covering spec 10.2 "listener.Route", "listener with doubles", "render" (with `FuzzDelivery` run for
  at least 30 s in CI mode), and `regclient` against an `httptest.Server`.
- The IPC contract test: every row of the `wiretest` IPC golden table, sent over a real socket to
  the listener, gets the table's response. `regclient` passes every row of the `wiretest` HTTP
  transcript table against an `httptest.Server` that replays it.
- No token or key in any IPC response or in `state.json` (I1), checked by a test that scans them.

**Must not touch:** `internal/registrar`, `internal/hook`, `internal/mcp`, `internal/setup`, any OS
implementation package, any `cmd/` file, any package 0 path.

---

## P3: harness adapters, plugin, Codex config, setup and doctor

**Owns:** `internal/hook/**`, `internal/mcp/**`, `internal/setup/**`, `plugins/claude/**`,
`.claude-plugin/**`, `integrations/codex/**`.

**Consumes:** `wire` (IPC messages), `ipc.Dial`, `paths.SocketPath`, `proc` (interface, for the
Windows client-sent chain).

**Provides:**

- `hook.Run` and its `Main`: parse stdin per harness and event, call the listener, print the exact
  bytes of spec 6.2 and 6.3, always exit 0, 0 bytes when empty, nothing when the listener is down.
- `mcp.Serve` and its `Main` (`kwak mcp --harness claude|codex`): the JSON-RPC server, the tool table
  of spec 6.4, a `Backend` that keeps one long IPC connection (hello first, then tools by id).
- The Claude Code plugin files and marketplace (spec 6.2); `claude plugin validate` must pass.
- The Codex snippets (spec 6.3).
- `setup.Main` for `kwak setup claude|codex [--write|--remove] [--exe <path>]`, the exported printer
  `setup.Print(w io.Writer, h wire.Harness) error` that P2's bare `setup` gets injected, and
  `setup.Doctor(d DoctorDeps)` returning the `Main` for `kwak doctor`. `DoctorDeps` injects
  `Preflight func() error` (P4's `oskeyring.Preflight`), `Secrets`, and the IPC status `Call`. Doctor
  checks bus, keyring, socket, PATH, plugin present, Codex hook trust entries and the Codex Windows
  hook shell, all read-only except `--write` and `--remove` into `$CODEX_HOME`.

**Acceptance tests that must pass:**

- `go test -race ./internal/hook/... ./internal/mcp/... ./internal/setup/...` covering spec 10.2
  "hook", "mcp" and "setup": exact stdout per harness and event for empty and non-empty replies;
  listener down gives exit 0 and 0 bytes within 400 ms; `stop_hook_active`; `agent_id` with no
  binding; MCP transcripts including `server/discover`, string ids, notifications, a 2 MB line,
  `_meta` pass-through; `--write` merge into a temp `CODEX_HOME`, idempotent on a second run.
- The mirror IPC contract test: `hook` and `mcp` send every request of the `wiretest` IPC golden
  table and, against a tiny in-test socket server that replays the table's responses, print exactly
  the expected bytes.
- `claude plugin validate ./plugins/claude` and `claude plugin validate .` pass (no model call, not
  billed). If `claude` is missing, the test skips with a message.

**Must not touch:** `internal/listener`, `internal/registrar`, `internal/render`, any OS implementation
package, any `cmd/` file, any package 0 path, the real `~/.claude` and `~/.codex`.

---

## P4: platform code (process ancestry, keyring, user service)

**Owns:** `internal/proc/osproc/**`, `internal/secrets/oskeyring/**`, `internal/service/**`.

**Consumes:** `proc` (interface), `secrets` (interface).

**Provides:**

- `osproc.New() proc.Reader`: Linux `/proc/<pid>/stat` (parse after the last `)`), macOS
  `unix.SysctlKinfoProc`, Windows Toolhelp32 plus `GetProcessTimes` with the "parent younger than
  child" rule (spec 5.7, go §4).
- `oskeyring.New(service, deadline) secrets.Secrets` with the goroutine deadline, and `Preflight()`
  (spec 5.1, go §1).
- `service.ForOS(runner, home) Manager` and its `Main` for `kwak install` and `kwak uninstall`:
  systemd user unit, launchd agent, Windows Run key with `--detach` and the `--supervise` loop
  (spec section 7). The Windows `listen --detach/--supervise` logic lives here as
  `service.Detach` and `service.Supervise`, called by P6's wiring. `Supervise` owns the Windows log
  (open, rotate at 5 MB, pass as the child's stderr). `Install` is idempotent and restarts a running
  listener (spec 7.4).

**Acceptance tests that must pass:**

- `go test -race ./internal/proc/osproc/... ./internal/secrets/oskeyring/... ./internal/service/...`:
  on Linux a real child's parent and start time, stat parsing with `)` and spaces, `Alive` false for a
  reaped child; the keyring deadline with a blocking inner function (the real keyring is not touched in
  tests; a test that does is behind `-tags keyring` and is run only by hand); the exact unit, plist and
  registry commands through a recording `Runner`, rendered for every OS on any OS.
- `make check` for all six pairs (this package has the most per-OS code).
- `systemd-analyze --user verify` on the rendered unit in a temp dir, skipped if unavailable.

**Must not touch:** everything outside its three directories. It never runs `systemctl --user enable`
or writes into the real `~/.config/systemd/user` in tests.

---

## P5: deployment artefacts (files and written steps, nothing applied)

**Owns:** `deploy/**`, `docs/deploy.md`.

**Consumes:** spec section 11 and 3.1, 3.7, 3.11.

**Provides:**

- `deploy/registrar/Dockerfile` (spec 11 step 3).
- `deploy/registrar/compose.example.yaml`: the Matrix host compose service, as a plain example and as the
  shape of a template for your own provisioning.
- `deploy/registrar/config.example.json` (spec 3.1).
- `deploy/registrar/registration.example.json` with placeholder tokens, and the tuwunel config change
  (`appservice_dir`, the step that writes `secrets/appservices/kwak.yaml`) written as a diff-shaped
  snippet in `docs/deploy.md`, not applied anywhere.
- `deploy/registrar/nginx-locations.example.conf`: the two reverse proxy locations (spec 11 step 7) and the
  upstream variable for `kwak_registrar_upstream`.
- `deploy/registrar/nftables.example.yml`: the `kwak-registrar-from-proxy` allowlist entry.
- `docs/deploy.md`: spec section 11 as numbered steps for the maintainer, with the apply order, the checks
  after each step, the secret-store entries to create, the room creation, owner
  commands, the first two-session test, backups and the incident steps.

**Acceptance tests that must pass:**

- `docker build -f deploy/registrar/Dockerfile .` succeeds offline only if the `golang:1.26` image is
  already present; otherwise the test is skipped and the step is listed for the maintainer. (No pulls in
  automated runs.)
- `docker compose -f deploy/registrar/compose.example.yaml config` parses.
- A grep test: no real token, key or password in any file under `deploy/` or `docs/deploy.md`.

**Must not touch:** any Go package, and any provisioning repo.
Nothing is applied to the Matrix host or the proxy.

---

## P6: integration (wiring and end-to-end)

Runs after P1 to P4 are merged.

**Owns:** `cmd/kwak/main.go` (final dispatch), `test/e2e/**`, `AUDIT.md`, and final edits to
`README.md`.

**Consumes:** everything.

**Provides:**

- `cmd/kwak/main.go`: the dispatch table from spec 5.13, building `listener.Deps` and `cli.Env` from
  the real implementations (`regclient`, `matrix.Client`, `osproc.New()`, `ipc.PeerPID`,
  `oskeyring.New`, `oskeyring.Preflight`, `service.ForOS`, `setup.Print`, an `os/exec` `Runner`, the
  real clock), and parsing `listen`'s flags, which dispatch to `service.Detach`, `service.Supervise`
  or `cli.Listen`.
- `test/e2e`: scenarios E1 to E26 (spec 10.4) on the `ittest` harness, with the registrar and two
  listeners in-process and fake harness processes made by re-executing the test binary
  (`KWAK_FAKE_HARNESS=1`), which start the real static `kwak` binary (shipped into the runner image
  through `ittest`'s extra-binaries option) as `kwak hook` and `kwak mcp` children, with
  `KWAK_SOCKET` set per owner.
- The registrar container smoke test: the real `kwak-registrar` binary as its own container (alias
  `kwak-registrar`) on the internal network, for one pairing and one agent.
- `AUDIT.md`: reading order, line counts per package (generated), which packages may touch the
  network or the keyring, and the dependency list with licences.
- The manual-check sheet for M1 to M8, with M5b (spec 10.6), written as steps. M8 needs a Windows
  machine and a teammate. Running them needs the maintainer's
  go-ahead (they are billed) and uses only `--plugin-dir`, `--settings` and a temporary `CODEX_HOME`.

**Acceptance tests that must pass:**

- `go test -race ./...`
- `go test -tags integration ./test/integration ./test/e2e`
- `make check`
- The I1 scan in E16.
- `go install ./cmd/kwak` from a clean checkout, then `kwak version` and `kwak doctor` run.

**Must not touch:** package internals owned by P1 to P4 (it reports bugs back to the owning package
instead), `~/.claude`, `~/.codex`, the Matrix host, the proxy.

---

## Order and hand-offs

1. P0 alone, until its acceptance tests and the fact gate pass. Output: a commit on `main`, and
   `FACTS.md`.
2. P1, P2, P3, P4 and P5 in parallel worktrees, each on a branch from P0's commit. Each merges when its
   acceptance tests pass. Merges cannot conflict, because the owned paths are disjoint.
3. P6 on `main` after the merges.
4. the maintainer decides the open questions of spec section 12 at any time. Each answer changes at most a
   default or one small behaviour; the package that owns it applies the change.
5. Deployment (P5's steps) is the maintainer's to run, after P6 passes.
