# Fact probes F1 to F13: measured answers

Measured on 2026-11-02 against `ghcr.io/matrix-construct/tuwunel:v1.9.4` (local image), server
`kwak.test`, with the config from `testdata/tuwunel.toml.tmpl`. The probes are
`probes_test.go` and `probe_helpers_test.go`. Each measurement is logged as a line that starts with
`FACT <id>`.

## What changed in the spec

**Nothing.** All 13 probes agree with the spec, so no fallback was applied. `docs/spec.md`,
`internal/wire` and `internal/matrix` are unchanged by this step. F4, F5 and F6 pass, so the gate
is open.

## How the probes ran, and the one caveat

- **The spec layout does not work on this laptop.** On the spec's internal bridge, `tuwunel`
  resolves to its address but TCP to port 8008 times out. The runner never gets past the 90 s
  readiness wait. I measured this again with F2 alone:
  `tuwunel resolves to [192.168.208.2] but TCP fails (dial tcp 192.168.208.2:8008: i/o timeout)`.
  This is the host firewall, not tuwunel. The cause needs root to read (`iptables -S FORWARD`,
  `iptables -S DOCKER-USER`, `iptables -S DOCKER-FORWARD`, `ipset list -n`). Nothing on the host
  was changed.
- **So every result below comes from the shared namespace layout**:
  `KWAK_IT_SHARED_NETNS=1 CGO_ENABLED=0 go test -tags integration ./test/integration/ -v -count=1`.
  The runner joins tuwunel's network namespace. Both names, `tuwunel` and `kwak-it-runner`, still
  resolve through Docker DNS (F2 logs that both resolve to the same address).
- **F2 caveat.** F2 measures that tuwunel's appservice client calls the runner **by docker name**,
  which is the fact the spec needs (matrix D5). It does not measure forwarding between two
  containers over a bridge. That is a property of the host, not of tuwunel, so the F2 fallback
  (`network_mode: container:tuwunel`) was **not** applied. Spec section 11 puts both containers on
  one compose network on the deployment host (read, not measured here). If the maintainer wants that measured, run F2 in the spec layout on a host
  whose firewall forwards between containers.
- The plain command `CGO_ENABLED=0 go test -tags integration ./test/integration/ -v -count=1`
  fails on this laptop for every test, at readiness, for the reason above. On such a host run
  `KWAK_IT_SHARED_NETNS=1 make integration`.

## Results

| Probe | Result | Measured answer |
|---|---|---|
| F1 | PASS | tuwunel loads the JSON `kwak.yaml`. `whoami` with the `as_token` is `@registrar:kwak.test`. Appservice `/register` of `agent.f1.listener` with device `KWAK-f1` works and the token works. |
| F2 | PASS (shared namespace, see caveat) | `POST /_matrix/client/v1/appservice/kwak/ping` answers 200 with `duration_ms`. tuwunel then calls `POST http://kwak-it-runner:8009/_matrix/app/v1/ping`. |
| F3 | PASS | Without `X-Forwarded-For`: appservice `/register` 500, appservice `/login` 500, human `/register` 500 "Can't extract client IP from configured ip_source". With it: all work (the human one reaches the 401 token stage). `whoami` without the header still answers 200. |
| F4 | PASS | Own-token join of a passwordless agent: 403 `M_FORBIDDEN` "Guests are not allowed to join this room". `as_token` plus `?user_id=` join: works. The agent then sends with its own token, and the event's sender is the agent. The 3.11 room has no `m.room.guest_access` (404). The refusal is tuwunel's guest check: it treats passwordless appservice accounts as guests. That is why the F4 fallback (`guest_access: can_join`) would let any such account join. |
| F5 | PASS | A passwordless `agent.x.listener` syncs with the literal 5.3 filter. It sees the root, a thread reply and a later message. The timeline holds only `m.room.message` (bob's join is dropped). A message in a second joined room is dropped by `room.rooms`. `/event/{id}`, the `m.thread` relations (1 reply) and `joined_members` all work with its own token. |
| F6 | PASS | `as_token` with `?user_id=&device_id=KWAK-a` logout: `KWAK-a` token dies (401 `M_UNKNOWN_TOKEN`), `KWAK-b` and the `as_token` keep working. A new login on `KWAK-a` gives a working token, and the old one stays dead. `?user_id=` `logout/all` kills both devices; the `as_token` survives. |
| F7 | recorded | A second login on device `KWAK-c` without a logout gives a new, different token, and **the old token keeps working** (whoami 200). So "log out first" in the registrar is needed, as the spec already does. |
| F8 | PASS | After appservice `/register` the display name is `agent.f8.0123456789abcdef 💕`. Setting it through `?user_id=` gives exactly the localpart. |
| F9 | PASS | `as_token` with `?user_id=@magent.x:kwak.test` (a registered human): **400** `M_EXCLUSIVE` "User is not in namespace", on `whoami` and on a display name change (the name stays unchanged). The same for `@alice`. Appservice `/register` of `magent.y`: 400 `M_EXCLUSIVE`. Control: an `agent.*` user in the namespace works. The spec names no status code; it is 400, not 403. |
| F10 | PASS | 150 joins between two messages: the next sync has `limited: true`, a non-empty `prev_batch`, and only the second message in its timeline. One `/messages?dir=b&limit=100` page with `{"types":["m.room.message"]}` from `prev_batch` finds the first. |
| F11 | PASS | The `as_token` creates the DM with the 3.5 body. Room version 12, power levels written with `"users":{}`. `RoomEncrypted` is false. alice's `m.room.encryption` is refused, 403 "sender does not have enough power (Int(0))". alice's message reaches the appservice in a transaction, and `Event(room, id)` with the `as_token` gives the same sender and body. |
| F12 | PASS | admin creates the room with the 3.11 body while `@registrar` is not a member. Version 12. Power levels hold `@registrar:kwak.test: 50` before and after it joins. History `shared`, join rule `public`, no guest_access, no encryption. The registrar resolves and joins `#agents:kwak.test`. At 50 it can kick, and it cannot change the topic (403). |
| F13 | PASS | A passwordless agent **can** change its own display name and avatar with its own token. Its `m.room.member` event (`displayname: alice`) reaches the appservice in a transaction. `SetDisplayName(lp, lp)` and `SetAvatarURL(lp, "")` through `?user_id=` restore the profile and the room member state (display name = localpart, no avatar). |

## Facts other packages need (measured, beyond the table)

- **Transactions and pings carry the `hs_token` both ways:** in `Authorization: Bearer` and in
  `?access_token=`. Both match. Spec 3.3's "if both are present, both must match" applies to every
  request.
- **`RoomEncrypted` without a trailing slash works** (`/state/m.room.encryption`). It says false for
  the DM and true for a room created with `m.room.encryption` in `initial_state`. No path change is
  needed in `internal/matrix`.
- **`SetAvatarURL("")` clears the avatar** on tuwunel (profile `avatar_url` empty, member state
  without it).
- **`ProfileExists` with the `as_token`:** true for a registered agent, false (a 404) for an unknown
  user.
- **`ittest.Env.Client` and `Env.AppserviceClient` set `AllowHTTP`**, so they talk to
  `http://tuwunel:8008`. `TestSmoke` checks both against tuwunel. The probes still build their own
  clients.
