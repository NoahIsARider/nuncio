# Deployment

nuncio is a single static binary with an embedded SQLite database and a local
object cache. There is no Postgres, no Redis, no queue and no telemetry.

Requirements: `git` 2.40 or newer on the host, and `ssh` for the forges you use.
Go is only needed to build from source.

## Five minutes, from an existing clone

```bash
cd my-project
nuncio init                 # reads git remote -v; writes nuncio.toml, dry_run = true
nuncio doctor               # lists missing tokens by name, never by value
nuncio plan                 # shows what would happen; changes nothing
nuncio sync                 # one shot, after the plan looks right
nuncio config set daemon.dry_run false
nuncio serve                # or leave the timer running
```

`init` detects the forge from `git remote -v` and scaffolds one platform block.
Add the rest explicitly:

```bash
nuncio init --forge github,codeberg --force
```

`init` overwrites nothing unless `--force` is passed.

## Credentials

Secrets are references in the config. Literal tokens are rejected.

```toml
[platforms.github]
kind = "github"
auth = "env:GITHUB_TOKEN"

[platforms.codeberg]
kind = "forgejo"
api  = "https://codeberg.org/api/v1"
git  = "ssh://git@codeberg.org"
auth = "ssh:codeberg"       # ~/.ssh/codeberg, IdentitiesOnly yes
```

| Platform | API token | Git transport | Minimum scopes |
|---|---|---|---|
| GitHub | `env:GITHUB_TOKEN` | HTTPS with token, or SSH | `repo`, `read:org`; protection toggling needs admin |
| GitLab | `env:GITLAB_TOKEN` | SSH, or HTTPS with token | `api` or `read_api` + `write_repository` |
| Forgejo/Codeberg | `env:FORGEJO_TOKEN` | SSH | `write:repository`, `write:user` |
| SourceHut | `env:SRHT_TOKEN` | SSH | `git.sr.ht/REPOSITORIES:RW`, `git.sr.ht/OBJECTS:RW` |
| Tangled | `env:TANGLED_TOKEN` (XRPC) | SSH to the knot | repository write |

Put them in an `EnvironmentFile` for systemd, or in the compose environment.
The daemon never writes a token to the state database or to a log line.

## The four shapes

All four are present, commented, in [`deploy/docker-compose.yml`](../deploy/docker-compose.yml).

### 1. Timer only — default

No ingress, no public address, no webhook. The daemon wakes every
`daemon.interval` and polls each forge.

```toml
[daemon]
listen = ""          # no listener at all
interval = "15m"
concurrency = 4
dry_run = true
```

This is the right shape for a laptop, a NAS, or a VPS behind NAT.

### 2. Timer + webhooks

One HTTPS path is exposed. Webhook deliveries only wake the scheduler; they
never carry a plan.

```toml
[daemon]
listen = "127.0.0.1:8787"
interval = "6h"       # a safety net, not the primary trigger

[platforms.github]
webhook_secret = "env:GITHUB_WEBHOOK_SECRET"
```

Point the forge at `https://your.host/hooks/github/<owner>/<name>`. The listener
binds loopback; terminate TLS in Caddy, nginx or a tunnel. nuncio verifies the
signature and refuses unsigned deliveries with 403.

### 3. Timer + firehose (Tangled)

No ingress at all. nuncio holds an outbound WebSocket subscription and filters
`#commit` frames by repository DID.

```toml
[platforms.tangled]
kind = "tangled"
knot = "tangled.org"
owner = "alice"
firehose = "wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos"
```

Cursor loss (`FutureCursor`, `ConsumerTooSlow`) is treated as a hole: the cursor
is dropped and every managed repository is reconciled.

### 4. Full

Webhooks plus firehose plus the timer as a backstop. Every event source goes
through the same per-repository coalescing scheduler, so a burst of events
produces one reconcile, not one per event.

## systemd

```bash
sudo useradd --system --home /var/lib/nuncio --shell /usr/sbin/nologin nuncio
sudo install -d -o nuncio -g nuncio /etc/nuncio /var/lib/nuncio
sudo install -m 0600 nuncio.toml /etc/nuncio/nuncio.toml
sudo install -m 0600 env /etc/nuncio/env       # GITHUB_TOKEN=..., etc.
sudo install -m 0644 deploy/nuncio.service /etc/systemd/system/nuncio.service
sudo systemctl daemon-reload
sudo systemctl enable --now nuncio
journalctl -u nuncio -f
```

`nuncio doctor --print-systemd` prints the unit inline. The unit runs as an
unprivileged user, with `ProtectSystem=strict`, `ProtectHome=true` and a single
writable path (`/var/lib/nuncio`). If your SSH keys live in `/home`, either copy
them into `/var/lib/nuncio/.ssh` or relax `ProtectHome` deliberately.

## Docker

```bash
docker build -f deploy/Dockerfile -t nuncio .
docker run --rm \
  -v "$PWD/nuncio.toml:/etc/nuncio/nuncio.toml:ro" \
  -v nuncio-state:/var/lib/nuncio \
  -e GITHUB_TOKEN \
  nuncio -c /etc/nuncio/nuncio.toml sync
```

The image deliberately includes `git` and `openssh-client`; a scratch image
would be smaller and useless. `nuncio doctor --print-docker` prints a compose
service inline.

For webhooks, publish **only** loopback and put a TLS terminator in front:

```yaml
ports:
  - "127.0.0.1:8787:8787"
```

Binding `0.0.0.0` is allowed but logs a warning; TLS is your responsibility.

## Reverse proxy

Caddy:

```caddyfile
nuncio.example.org {
    reverse_proxy 127.0.0.1:8787
}
```

nginx:

```nginx
server {
    listen 443 ssl http2;
    server_name nuncio.example.org;
    location / {
        proxy_pass http://127.0.0.1:8787;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        client_max_body_size 2m;
    }
}
```

## State and backup

| Path | Contents |
|---|---|
| `state.path` (default `.nuncio/state.db`) | SQLite: observations, journal, snapshots, reports, webhook dedupe |
| `state.cache` (default `.nuncio/repos`) | one bare repository per managed repo; all objects ever observed |

Back up both. The cache is the reason the lost-commit proof can be honest: it
keeps objects that a forge has already forgotten. A cache that is deleted still
leaves the state database, but `lost` will report `unknown` until the objects are
seen again, rather than claiming zero.

The SQLite database is safe to copy while the daemon is stopped. For a live
copy, use `sqlite3 state.db ".backup backup.db"`.

## Operating

```bash
nuncio doctor                    # capability matrix for your tokens
nuncio plan                      # live plan, no writes
nuncio sync                      # one reconciliation
nuncio report                    # live report
nuncio report --since 24h        # stored runs
nuncio lost                      # the proof on its own
nuncio snapshot list --repo alice/example
nuncio snapshot create --repo alice/example --ref refs/heads/main
nuncio snapshot restore --repo alice/example --name refs/nuncio/snapshot/main/1759000000
```

Exit codes are contractual:

| Code | Meaning | Cron behaviour |
|---|---|---|
| 0 | clean | nothing to do |
| 1 | findings for you (divergence parked, ref skipped, deletion reported) | notify, not page |
| 2 | operational error (auth, network, unreachable platform) | retry later |
| 3 | safety refusal (a destructive action lacked a snapshot) | page immediately |

A platform that fails to answer is `unknown`; it is never treated as empty. An
outage cannot be mistaken for a mass deletion.

## Verifying a deployment

1. `nuncio doctor` — every platform says `api ok` and the capability rows match
   what you expect.
2. `nuncio plan` — the actions are the ones you intended.
3. `nuncio sync` — exit 0 or 1, never 3.
4. `nuncio report` — `lost commits 0` and no `to resolve` section.
5. `git ls-remote` on each forge — the refs match.
6. `curl -s localhost:8787/status.json` — each repo has a recent `last_run`.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `api FAIL ... 401` | token absent, expired, or wrong scope |
| `api FAIL ... 404` on a private repo | token cannot see the repository |
| platform shows `unreachable` in a report | auth/network; the platform is not deleted |
| `lost commits unknown` | cache objects are missing; restore the cache backup |
| `snapshot ... already exists with a different SHA` | two snapshots collided in the same second; re-run, the tag includes the timestamp |
| `gate UNVERIFIED` in doctor | expected: the sr.ht pre-receive gate is warn-only |
| `daemon.dry_run = true` in sync | intended first-run safety; flip it explicitly |
