# Working on kwak

This guide is for people who want to change kwak, test a change, or try kwak without touching
anyone's real setup, including their own. It uses three things that exist for this (decisions 39
and 40 in `docs/spec.md`):

- a **dev build** of `kwak`, which keeps its own folders and never touches your real pairing;
- a **test pairing**, which only reaches the test rooms `#agents-test` and `#agents-report-test`;
- **throwaway sessions** of Claude Code or Codex that talk to the dev listener only.

When this guide says "ask the maintainer", the answer is not in the repo: the maintainer decides.

## What you need

- Go, at least the version in `go.mod`.
- A clone of the repo:

  ```sh
  git clone https://github.com/fcanela/kwak.git
  cd kwak
  ```

- For a test pairing: an owner account on the kwak server, and the server's URL. The server admin
  adds owners (`docs/deploy.md`, section 8). Ask the maintainer.
- For `make integration` only: Docker (see below).

## Build and run the unit tests

```sh
make test     # go test -race ./...: every unit test, no network, no containers
make check    # go vet (also with the integration tag), then a build for 6 OS and CPU pairs
make build    # go build ./...
```

Both `make test` and `make check` must pass before you hand in a change. The unit tests use
hand-written doubles passed in through interfaces (spec 10.1): no mocking libraries, no swapped
package variables. They never touch your keyring, your harness config or a server.

## Integration tests (they start containers)

The tests never download images (`docker run --pull never`), so fetch the pinned tuwunel image
once first. The tag is `TuwunelImage` in `test/integration/ittest/ittest.go`:

```sh
docker pull ghcr.io/matrix-construct/tuwunel:v1.9.4
```

```sh
make integration    # ./test/... and ./deploy/ with the integration tag
make it-registrar   # the registrar's integration tests
```

These **start Docker containers on your machine**: a real tuwunel Matrix server on an internal
Docker network, plus a runner image built from the test binary, and `make integration` also builds
the registrar image and checks the nginx config. They use your Docker and take a while. Most changes do not need them; run them when you change how kwak talks to
the Matrix server or the registrar. On a machine whose firewall drops traffic between containers:

```sh
KWAK_IT_SHARED_NETNS=1 make integration
```

## The dev build

```sh
make dev            # builds bin/kwak
./bin/kwak version
```

`make dev` builds `bin/kwak` with a link-time flag that marks it as a dev build. A dev build:

- uses its own folders, named `kwak-dev` where a normal build uses `kwak`. On Linux:

  | What | Normal build | Dev build |
  |---|---|---|
  | config | `~/.config/kwak/` | `~/.config/kwak-dev/` |
  | state and logs | `~/.local/state/kwak/` | `~/.local/state/kwak-dev/` |
  | socket | `/run/user/<uid>/kwak/kwak.sock` | `/run/user/<uid>/kwak-dev/kwak.sock` |

  On macOS and Windows the same rule applies (spec 5.6). `./bin/kwak version` prints the folders it
  uses.
- keeps its owner key in its own keyring item (each pairing has its own item, spec 5.2), so it
  never reads or overwrites your real key;
- refuses `kwak install`, `kwak uninstall` and bare `kwak setup`: it never touches the user service
  of your real listener. You run the dev listener in a terminal instead (below);
- refuses `kwak setup codex --write` and `--remove` unless `CODEX_HOME` is set, so it never edits
  your real `~/.codex`;
- says so in `kwak version`, `kwak status` and `kwak doctor`.

`go install github.com/fcanela/kwak/cmd/kwak@<version>` and the Nix package never set the flag, so
they always make a normal build.

To point a dev build at a real pairing on purpose, set `KWAK_CONFIG_DIR`, `KWAK_STATE_DIR` and
`KWAK_SOCKET` to that pairing's folders. Nothing else does it. You should almost never need this.

### Use the dev build in a terminal

The harness hooks and the MCP server run `kwak` from your `PATH`. So in each terminal you use for
dev work, put `bin` first:

```sh
cd <your clone>
export PATH="$PWD/bin:$PATH"
kwak version        # must say (dev build)
```

Terminals without this line keep using your normal `kwak` and your real pairing.

## Get a test pairing

In a dev terminal (with `PATH` set as above):

```sh
kwak pair --server https://<the kwak server> --owner <your owner name>
```

It prints a line like `pair test 7H3Q-9XRM-K2PD`. Send that line to `@registrar` in your chat with
it in Element. The bot answers that this is a test pairing. Then:

- its agents and its own listener account are joined only to `#agents-test`, never to `#agents`.
  Each pairing has its own listener account, named with a random word pair; a test pairing's name
  ends in `-test`, for example `agent.<owner>.calm-otter-test.listener`. The bot's reply names it,
  and `kwak status` shows it ("Listener account: ...") once the listener runs;
- its reports go only to `#agents-report-test`, never to `#agents-report`.

If the bot says "Test pairings are off on this server", the server has no test room yet. Ask the
maintainer.

Pair for tests only with a kwak from this repository (a `make dev` build). An older kwak does not
know test pairings: it accepts a `pair test <code>` answer with no error, does not show it as a
test pairing, and keeps the key under the one keyring name all its pairings share, so a second
pairing on the same machine overwrites your real pairing's key. `kwak status` of a new kwak says
"(a test pairing: only the test rooms)" when it worked.

Plain `pair <code>` makes a normal pairing, even from a dev build. The dev build then tells you its
agents join the main agents room. To switch to a test pairing, run `kwak unpair`, then `kwak pair`
again and send `pair test <code>`. (Sending `pair test` on a re-pair, without `kwak unpair` first,
also works, but the earlier normal pairing then stays live at the registrar: `kwak pair` and the bot
both tell you to send `revoke <old id>` if no machine needs it.)

## Run the dev listener in the foreground

In a dev terminal of your desktop session (the listener needs your keyring):

```sh
kwak listen
```

It logs to this terminal. Stop it with Ctrl-C. Check it from another dev terminal:

```sh
kwak status     # "Dev build: ...", "Pairing: ... (a test pairing: only the test rooms)", "Listener account: ..."
kwak doctor
```

Your real listener keeps running as before: different socket, different folders, different key.

## Open a throwaway session against it

Sessions are billed model runs. Keep them short.

Start them in a throwaway directory, from a dev terminal, so their hooks and tools run the dev
`kwak`:

```sh
export T=$(mktemp -d)
mkdir -p "$T/work" && cd "$T/work"
```

**Claude Code.** Write a settings file with kwak's allow rule, so its tools do not prompt:

```sh
cat > "$T/claude-settings.json" <<'EOF'
{"permissions":{"allow":["mcp__plugin_kwak_kwak__*"]}}
EOF
```

- If the kwak plugin is **not** installed for your user:

  ```sh
  claude --plugin-dir <your clone>/plugins/claude --settings "$T/claude-settings.json"
  ```

- If the kwak plugin **is** installed (marketplace or Nix), do not add `--plugin-dir`: two copies
  make every hook fire twice. The installed plugin runs `kwak` from `PATH`, which is the dev build
  in this terminal:

  ```sh
  claude --settings "$T/claude-settings.json"
  ```

Check: `kwak status` lists the session and its agent.

**Codex.** Use a throwaway `CODEX_HOME`. It has no login yet; `codex login` writes only into it.

```sh
export CODEX_HOME="$T/codex"
mkdir -p "$CODEX_HOME"
codex login
kwak setup codex --write
codex
```

The first time, choose "Trust all and continue" for the kwak hooks. `kwak doctor` (with the same
`CODEX_HOME`) shows whether they are trusted.

## The test rooms

| Room | Who is in it | What it is for |
|---|---|---|
| `#agents-test` | test pairings' agents and their listener account | the agents room of test pairings |
| `#agents-report-test` | the account posting a report, only while it posts (agents and listener accounts leave after each report) | reports from test pairings: a check that stopped a message (with its text and a link, except after the secrets scan), the hourly limit of background runs |

A test pairing can only reach these two rooms. The registrar decides this, not your machine, so a
change to kwak on your machine cannot send a test agent into `#agents`. To read the test rooms in
Element, join them as yourself; if a room is invite-only, ask the maintainer to invite you.

## Try a change live without touching anyone's setup

1. Make the change, and run `make test` and `make check`.
2. `make dev`, and use a dev terminal (`export PATH="$PWD/bin:$PATH"`).
3. Use a test pairing (`pair test <code>`), never your real one.
4. Run the dev listener with `kwak listen` in the foreground. Restart it after each `make dev`.
5. Open throwaway sessions in `$T/work`, as above. Talk to them from Element in `#agents-test`, or
   from a second throwaway session.
6. Watch the dev listener's output, `kwak status` and `kwak inbox`.

Things that would touch a real setup, and how this guide avoids them:

- `kwak install` and `kwak uninstall` change the real user service: a dev build refuses them.
- `~/.codex` is the real Codex config: use a throwaway `CODEX_HOME`.
- `~/.claude/settings.json` is the real Claude Code config: pass `--settings` with a temp file
  instead of editing it.
- Settings in your DM with `@registrar` (for example `set question-timeout ...`) are owner
  settings. They apply to all your pairings, the real one too. Change them only if you mean that.
  Background resume is not one of them: `kwak config resume` sets it in the `config.json` of the
  build you run, so a dev build's setting stays in its own folder.
- The registrar and the Matrix server are shared. Changes there are deployed by the maintainer
  (`docs/deploy.md`). Ask the maintainer.

## Manual checks

[docs/manual-checks.md](manual-checks.md) has the checks that need a real Claude Code or Codex
(spec 10.6). Most of them are **billed model runs**: each check says "billable" or "not billable".
Run them only with the maintainer's go-ahead. M17 runs a test pairing with a dev build, step by
step.

## Clean up

In a dev terminal:

```sh
kwak unpair                       # revokes the test pairing, deletes its key, config and state
rm -rf ~/.config/kwak-dev ~/.local/state/kwak-dev
rm -rf "$T"
unset CODEX_HOME T
```

Your real pairing, its key and its service are not touched.

## Rules for contributors (from the repo)

- **The spec is the source of truth.** `docs/spec.md` cites the maintainer's decisions as
  "decision N". A change that departs from a decision is marked **DEVIATION (decision N): ...
  because ...** where it applies and listed in the DV table (spec 1.4).
- **Open choices go to the Q table** (spec section 12). A choice you had to make while building is
  written there as "As built, his to confirm", and an answer the maintainer gives later as
  "Answered by decision N".
- **VERIFY** marks a claim read in source or docs but not measured yet, with the test that settles
  it (spec, "Labels used below").
- **State files change only by adding.** A new key in `config.json` or `state.json` must be one an
  older kwak can ignore and keep (spec 5.2, 7.4). The registrar reads its own files strictly, so a
  new registrar key needs the deploy order of `docs/deploy.md` section 12.
- **Tests** use Go's `testing` only, with hand-written doubles passed in through interfaces; no
  mocking libraries, no swapped package variables, and `go test -race ./...` passes (spec 10.1).
- **Dependencies**: the standard library, plus `github.com/zalando/go-keyring` and
  `golang.org/x/sys`. Any other dependency needs a written reason and the maintainer's yes
  (`docs/plan.md`).
- **Docs and messages** are in plain English: short sentences, no em-dashes (`docs/plan.md`).
- **Never** name the real server in tests, never touch `~/.claude`, `~/.codex`, the Matrix host or
  the proxy from a test, and never run a billable harness without the maintainer's go-ahead
  (`docs/plan.md`).
- Branches, commits and pull requests: ask the maintainer.
