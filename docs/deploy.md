# Deploying kwak

This guide sets up the server side of kwak: a Matrix homeserver (tuwunel), the kwak registrar next
to it, and a reverse proxy in front. It follows spec section 11. Names and addresses here are
examples. Replace them with yours:

| Example | Meaning |
|---|---|
| `kwak.example.org` | your Matrix server name |
| `@alice:kwak.example.org` | a Matrix ID |
| `192.0.2.4` | the address of the reverse proxy |
| `192.0.2.12` | the address of the Matrix host, as the proxy reaches it |
| `/srv/tuwunel` | the directory of the tuwunel compose stack |
| `/srv/kwak-registrar` | the directory of the registrar compose stack |

The ready files are in `deploy/registrar/`:

| File | What it is |
|---|---|
| `Dockerfile` (+ `Dockerfile.dockerignore`) | The registrar image: a static binary on `scratch`. |
| `compose.example.yaml` | The registrar stack with example values. |
| `compose.netns-fallback.example.yaml` | Only if the ping check in step 3 fails. |
| `config.example.json` | The registrar's config (spec 3.1). Not secret. |
| `registration.example.json` | The shape of the registration, with placeholders. The real one comes from step 1. |
| `nginx-locations.example.conf` | The two reverse proxy locations. |
| `nftables.example.yml` | The firewall rule for port 8010, as data. |

`go test ./deploy/` checks these files: no real secret in them, the config and registration agree
with the code, and `docker compose config` accepts both compose files.
`go test -tags integration ./deploy/` also builds the image and runs `nginx -t` on the locations,
with local images only.

## 0. Before you start

You need:

- A tuwunel homeserver that runs in docker compose, with a docker network the registrar can join.
- A human admin account on it (for example `@alice:kwak.example.org`).
- A reverse proxy that serves your Matrix server name over HTTPS.
- A secret store: a password manager, `pass`, a vault, or a root-only file. You will keep two
  things in it: the registration (step 1) and, optionally, your Matrix access token (step 6).
- The `kwak-registrar` binary on your own machine, for steps 1 and 6. Build it from the repo, or:

  ```
  go install github.com/fcanela/kwak/cmd/kwak-registrar@latest
  ```

The registration is the most powerful kwak secret: its `as_token` acts as every agent and as
`@registrar`. Never put it in git, in a Matrix room, in shell history or in a chat. Never use a
tuwunel admin command that echoes appservice config (the room history would keep it).

## 1. Tokens

The registration goes straight from the generator into your secret store. It is never printed:

```
kwak-registrar gen-registration -url http://kwak-registrar:8009 \
  -server kwak.example.org -o - \
  | <store the note>
```

`<store the note>` is the command of your store that reads stdin and saves it, for example
`pass insert -m kwak/registration` or `install -m 0400 /dev/stdin /path/kwak.yaml`.

`-o -` refuses to write to a terminal, so a missing pipe fails instead of showing the tokens.

**Check** (prints the shape, never a token). Feed the stored registration to this on stdin:

```
<read the note> | python3 -I -c '
import json, sys
r = json.load(sys.stdin)
print(r["id"], r["url"], r["sender_localpart"], len(r["as_token"]), len(r["hs_token"]),
      r["io.element.msc4190"])'
```

Expected: `kwak http://kwak-registrar:8009 registrar 64 64 False`.

## 2. tuwunel: load the appservice

tuwunel loads appservices **only at boot**. Never also add kwak with `!admin appservices register`:
the same id twice fails with "Duplicate id".

1. Make a directory for registrations in the tuwunel stack, for example
   `/srv/tuwunel/secrets/appservices`. The container must be able to read it.
2. Write the registration there as `kwak.yaml`. It is JSON, which tuwunel reads as YAML. The file
   must be mode **0400**, owned by the uid tuwunel runs as in the container. Mount the directory,
   not the single file.
3. Tell tuwunel where it is, in `tuwunel.toml`. A misspelled key stops the server, because
   `error_on_unknown_config_opts` is on:

   ```toml
   # Appservices load at boot only. Never also register one with !admin appservices register.
   appservice_dir = "/run/secrets/appservices"
   ```

   The path is the one inside the container. In this example the compose file mounts
   `./secrets:/run/secrets:ro`.
4. Restart the tuwunel stack.

**Check after the restart.** On the Matrix host:

```
docker compose -f /srv/tuwunel/compose.yaml ps
docker compose -f /srv/tuwunel/compose.yaml logs --since 10m tuwunel | grep -i -E 'appservice|error'
```

A bad registration file stops the server, so `ps` must show it running. Then run the **as_token
check** below. It reads the token on the Matrix host, inside one process, so the token never
reaches argv, the terminal or shell history:

```
sudo python3 -I - <<'EOF'
import json, sys, urllib.request, urllib.error
reg = json.load(open("<tuwunel stack dir>/secrets/appservices/kwak.yaml"))
def call(method, path, body=None):
    req = urllib.request.Request(
        "http://127.0.0.1:8008" + path, method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + reg["as_token"],
                 "X-Forwarded-For": "127.0.0.1", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=40) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
print("whoami", *call("GET", "/_matrix/client/v3/account/whoami"))
if sys.argv[1:] == ["ping"]:
    print("ping", *call("POST", "/_matrix/client/v1/appservice/kwak/ping", {}))
EOF
```

Use your real stack directory in place of `<tuwunel stack dir>`, and the port tuwunel listens on
if it is not 8008.

Expected: `whoami 200 {"user_id":"@registrar:kwak.example.org",...}`. tuwunel created `@registrar`
at boot (fact F1).

## 3. The registrar container

Make a stack directory, for example `/srv/kwak-registrar`, and a system uid for it (1 to 999):

```
/srv/kwak-registrar/
  compose.yaml                     root, 0640
  config.json                      root:<gid>, 0640, not secret
  secrets/appservices/kwak.yaml    <uid>, 0400, the same registration as step 2
  data/state.json                  <uid>, written by the registrar
  src/                             a git checkout the image is built from
```

`data` is owned by the uid, mode 0700. `secrets` and `secrets/appservices` are readable by the
group.

**Build the image** from the repo. Any commit or tag works. Pinning to a release tag is optional,
but it makes a rebuild repeat the same image:

```
git clone https://github.com/fcanela/kwak.git /srv/kwak-registrar/src
cd /srv/kwak-registrar/src
git checkout v0.1.0      # optional: any tag or commit
docker buildx build -f deploy/registrar/Dockerfile -t kwak-registrar:v0.1.0 .
```

Use BuildKit (`docker buildx`), so `deploy/registrar/Dockerfile.dockerignore` applies. For a
private repo, clone over ssh with a read-only deploy key.

**`compose.yaml`** is `deploy/registrar/compose.example.yaml`:

```yaml
services:
  kwak-registrar:
    image: kwak-registrar:v0.1.0
    restart: unless-stopped
    user: "999:999"
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    pids_limit: 256
    command: ["serve", "-config", "/etc/kwak-registrar/config.json"]
    volumes:
      - ./config.json:/etc/kwak-registrar/config.json:ro
      - ./secrets:/run/secrets:ro
      - ./data:/var/lib/kwak-registrar
    networks:
      tuwunel:
        aliases: [kwak-registrar]
    # 8009 (the appservice side) is never published.
    ports:
      - "127.0.0.1:8010:8010"
      - "192.0.2.12:8010:8010"
networks:
  tuwunel:
    external: true
    name: tuwunel_default
```

- `user` is the uid and gid you made. It must match the owner of `kwak.yaml` and `data`.
- The container is read-only, drops all capabilities and gets no new privileges.
- `tuwunel_default` is the docker network of the tuwunel stack. Use its real name. The registrar
  reaches tuwunel there as `http://tuwunel:8008`, and tuwunel reaches the registrar as
  `http://kwak-registrar:8009`, through the alias.
- Port 8009 is never published. Port 8010 is published only on loopback and on the address the
  reverse proxy connects to (`192.0.2.12`). Never publish it on `0.0.0.0`.

**`config.json`** (spec 3.1) is `deploy/registrar/config.example.json`:

```json
{
  "server_name": "kwak.example.org",
  "homeserver": "http://tuwunel:8008",
  "registration": "/run/secrets/appservices/kwak.yaml",
  "state": "/var/lib/kwak-registrar/state.json",
  "appservice_addr": ":8009",
  "api_addr": ":8010",
  "room": "#agents:kwak.example.org",
  "admins": ["@alice:kwak.example.org"],
  "idle_after": "336h",
  "close_grace": "10m",
  "key_lifetime": "2160h",
  "key_warn_before": "336h",
  "pairing_ttl": "10m",
  "unconfirmed_ttl": "10m",
  "prune_closed_after": "2160h",
  "jobs_every": "1h"
}
```

The image has no shell and no CA certificates. It needs none: the registrar only talks to tuwunel
over plain HTTP inside the docker network (spec 3.8, D1).

Start it with `docker compose -f /srv/kwak-registrar/compose.yaml up -d`.

**Check after the container starts:**

```
docker compose -f /srv/kwak-registrar/compose.yaml ps
docker compose -f /srv/kwak-registrar/compose.yaml logs --since 10m
curl -s http://127.0.0.1:8010/_kwak/v1/me
```

- `ps` shows it running, not restarting. A `state.json` that does not parse stops it on purpose.
- The log says it joined the agents room. Before step 6 it retries every 5 s until the room exists.
- `curl` answers `401` with `KWAK_BAD_KEY`.

### The ping check (do not skip)

On some hosts the firewall drops TCP between two containers on one bridge
(`test/integration/FACTS.md`, F2 caveat). Name resolution works, but connections time out. Whether
your host forwards between containers on `tuwunel_default` is not known until you measure it. This
check measures it: tuwunel calls the registrar by its docker name, the same path every transaction
takes.

Run the as_token check from step 2 with the `ping` argument: replace `sudo python3 -I - <<'EOF'`
with `sudo python3 -I - ping <<'EOF'`.

- **Pass:** `ping 200 {"duration_ms":...}`. tuwunel reached `http://kwak-registrar:8009`.
- **Fail:** a 502 or 504 (`M_CONNECTION_FAILED`, `M_CONNECTION_TIMEOUT` or similar) after up to
  about 30 s.

If it fails, first rule out a registrar that is not listening. This reaches the container from the
host, not from another container:

```
ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
  "$(docker compose -f /srv/kwak-registrar/compose.yaml ps -q kwak-registrar)")
curl -s -m 5 -o /dev/null -w '%{http_code}\n' -X POST "http://$ip:8009/_matrix/app/v1/ping"
```

`403` means the registrar listens and refused a request with no `hs_token`, as it should. Then the
problem is forwarding between containers, and the fallback below applies. To see the cause (read
only): `sudo nft list ruleset | grep -n -i -E 'forward|docker'`.

### Fallback: share tuwunel's network namespace

Use this only if the ping check fails while the host reaches port 8009. It is the layout the
integration tests ran in (`KWAK_IT_SHARED_NETNS=1`), where F2 passed.

The registrar joins tuwunel's network namespace with `network_mode: container:<tuwunel
container>`. Docker then allows no `ports`, `networks` or aliases on the registrar, so the tuwunel
stack carries them:

1. In the `tuwunel` service of the tuwunel compose file. The alias keeps the registration URL
   `http://kwak-registrar:8009` valid, so no new registration is needed:

   ```yaml
       networks:
         default:
           aliases: [kwak-registrar]
       ports:
         - "192.0.2.12:8008:8008"
         - "192.0.2.12:8010:8010"
   ```

2. The registrar's compose file becomes `deploy/registrar/compose.netns-fallback.example.yaml`:
   no `networks`, no `ports`, no top-level `networks`, and

   ```yaml
       network_mode: "container:tuwunel-tuwunel-1"
   ```

   That name is an example. Read the real one from
   `docker compose -f /srv/tuwunel/compose.yaml ps`.
3. `config.json` does not change: `http://tuwunel:8008` still resolves inside the shared namespace.
4. Recreate the tuwunel stack, then the registrar, and run the ping check again.

The cost: when the tuwunel container restarts or is recreated, the registrar loses its network.
After every tuwunel restart or recreate, run
`docker compose -f /srv/kwak-registrar/compose.yaml up -d --force-recreate`.

Boot is a second cost. Such a container cannot start before tuwunel runs, and dockerd does not
retry a container that failed to start while it restores them at boot. After the first reboot
with the fallback, check that both containers came up. If the registrar did not, weigh that before
keeping the fallback.

## 4. Firewall

Only the reverse proxy may reach port 8010. Without a rule, any host that can reach 8010 writes
its own `X-Forwarded-For`, which the registrar uses for the new-IP notices.

`deploy/registrar/nftables.example.yml` holds the rule as data: tcp, port 8010, source
`192.0.2.4/32` (the proxy). Write it in your firewall's own form. For example, with nftables, in
the input chain:

```
ip saddr 192.0.2.4 tcp dport 8010 accept
tcp dport 8010 drop
```

With ufw:

```
ufw allow from 192.0.2.4 to any port 8010 proto tcp
```

Docker publishes ports through its own chains, so check the rule really applies to published
ports on your host (for example in the `DOCKER-USER` chain).

**Check** from a host that is not the proxy: `curl -s -m 5 http://192.0.2.12:8010/_kwak/v1/me`
must time out. From the proxy it answers `401`.

## 5. Order of work

1. The firewall rule (step 4), before anything publishes 8010.
2. tuwunel: load the appservice and restart it (step 2). tuwunel queues transactions until the
   registrar is up. Run the step 2 checks.
3. The agents room (step 6).
4. The registrar container (step 3). Run its checks, **including the ping check**.
5. The reverse proxy (step 7).

## 6. The agents room

Create it as a human admin account, never as `@registrar`: in room version 12 the creator can never
be demoted, so whoever holds the `as_token` would own the room for ever (spec 3.11).

`create-room` reads your access token from stdin, never from argv. Either paste it (then Ctrl-D):

```
kwak-registrar create-room -homeserver https://kwak.example.org
```

or keep it in your secret store and pipe it:

```
<read the token> | kwak-registrar create-room -homeserver https://kwak.example.org
```

Element shows the token under Settings, Help & About, Advanced. The reverse proxy must route
`/_matrix/client/`, so `createRoom` goes through it like any client.

Or by hand in Element: a room named `agents`, public join rule, not in the directory, address
`#agents:kwak.example.org`, encryption off. Then in devtools set `m.room.power_levels` to the body
of spec 3.11 (`@registrar` at 50, `state_default` 100, `notifications.room` 100, and the listed
events at 100).

**Check:** within 5 s the registrar log says it joined. In Element, the member list shows
`@registrar` with power 50, and the room settings show no encryption and no guest access.

## 7. Reverse proxy

Add two locations to the server block that serves your Matrix server name. They are in
`deploy/registrar/nginx-locations.example.conf`, which also shows the `matrix_auth` rate limit
zone they use:

```nginx
# The only route that needs no key. It keeps one rate limit.
location = /_kwak/v1/pair {
  limit_req zone=matrix_auth burst=10 nodelay; limit_req_status 429;
  client_max_body_size 64k;
  proxy_pass http://192.0.2.12:8010; proxy_http_version 1.1;
  proxy_set_header Host $host; proxy_set_header X-Forwarded-For $remote_addr;
  proxy_set_header X-Forwarded-Proto https; proxy_read_timeout 30s; }

# Everything else under /_kwak/v1/. No limit: /pair/claim is a long poll and /agents takes bursts.
location ^~ /_kwak/v1/ {
  client_max_body_size 64k;
  proxy_pass http://192.0.2.12:8010; proxy_http_version 1.1;
  proxy_set_header Host $host; proxy_set_header X-Forwarded-For $remote_addr;
  proxy_set_header X-Forwarded-Proto https; proxy_read_timeout 60s; }
```

- `X-Forwarded-For` is overwritten with `$remote_addr`, never appended.
- `proxy_pass` has no URI part.
- `/_matrix/app/` is never routed. Your catch-all `location /` should return 404 for it.
- If you refuse `?access_token=` at server level, keep that. kwak sends secrets only in headers.

**Check** from anywhere:

```
curl -s https://kwak.example.org/_kwak/v1/me
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://kwak.example.org/_matrix/app/v1/ping
```

The first answers `401` with `KWAK_BAD_KEY`, which proves the route. The second answers `404`.

## 8. Owners

After start, the bot opens a DM with each admin from `config.json`. In that DM send:

```
owner add @alice:kwak.example.org
```

The owner name is the localpart, `alice`. Add more people the same way.

**Check:** the bot replies that the owner was added.

## 9. Machines and the first test

On each machine that runs coding sessions:

```
go install github.com/fcanela/kwak/cmd/kwak@latest
kwak setup
```

Run `kwak setup` in your own terminal: it pairs this machine, and you confirm the code in the bot
DM. It reads the defaults `KWAK_SERVER` (for example `kwak.example.org`) and `KWAK_OWNER` (for
example `alice`) from the environment, so you can set them once instead of passing flags.

Then, for Claude Code, install the plugin from the marketplace:

```
claude plugin marketplace add fcanela/kwak
claude plugin install kwak@kwak
```

or load it from a checkout with `claude --plugin-dir <path to the plugin>`. Use one way, never
both. Add `mcp__plugin_kwak_kwak__*` to `permissions.allow` in `~/.claude/settings.json` (Q9 (a)).

For Codex: `kwak setup codex`, then trust the hooks once.

**First test:** two sessions on one machine. One asks a question in the agents room, the other
answers it (e2e E17 covers the same path). Then pair a second machine or person.

## 10. Incidents

**Registrar or Matrix host compromise:**

1. Make a new registration and replace the stored one. As in step 1, it goes straight into your
   secret store and is never printed:

   ```
   kwak-registrar gen-registration -url http://kwak-registrar:8009 \
     -server kwak.example.org -o - \
     | <replace the note>
   ```

   Then run the check from step 1.
2. Write the new registration to both stacks (step 2 and step 3) and restart tuwunel, then the
   registrar. tuwunel loads the new tokens. The old `as_token` stops working.
3. In the bot DM, `revoke <owner> all` for every owner. That closes every agent and logs out every
   listener device.
4. Owners pair again (`kwak pair`).

**Suspected leak of one owner key:** the owner sends `revoke <pairing-id>` in the DM (or an admin
sends `revoke <owner> all`), then pairs again. A re-pair alone is a renewal: it keeps the agents
alive and their tokens valid (spec 3.2).

## 11. Backups

Copy the registrar state right after the tuwunel database backup, so the two restore as a pair.
`state.json` is replaced by rename, so a copy never sees half a file. For example, at the end of
your backup script:

```bash
kwak_data=/srv/kwak-registrar/data
if [[ -d "$kwak_data" ]]; then
  rsync -rlt --delete "$kwak_data/" "$BACKUP_DIR/kwak-registrar-backup/"
fi
```

**Check:** run the backup, then `ls -l` the backup directory. It shows `state.json`.

**Restore** the registrar state and the tuwunel database as a pair:

1. Stop the registrar, then restore tuwunel from its backup.
2. Copy `state.json` back to `/srv/kwak-registrar/data/`, owner the registrar's uid, mode 0600.
3. Start tuwunel, then the registrar, and run the step 3 checks.

If only tuwunel is restored, existing agents keep working: names tuwunel has but the registrar
lacks fall back to the clash path. Keep a copy of the backups off the Matrix host.
