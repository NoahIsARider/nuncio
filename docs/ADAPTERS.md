# Adapters

nuncio talks to a forge through a deliberately small interface. Every adapter
declares what it can and cannot do before the reconciler is allowed to act. The
matrix is not documentation that drifts from the code: the planner consults the
same `caps.Capabilities` value that `nuncio doctor` prints.

## Verification status

This build was produced without live forge credentials. No request in this
repository was sent to github.com, gitlab.com, codeberg.org, git.sr.ht or a
Tangled knot. What *was* verified:

- every adapter is exercised against `httptest` fixtures with the request and
  response shapes listed below, including error mapping (401/403/404/409/422);
- the reconciler and executor are exercised against a hostile fake-forge matrix
  (48 profiles × 9 scenarios) plus crash recovery after every journalled prefix;
- plain-git adapters are exercised end to end against two local bare
  repositories, including force-pushes, snapshots and the lost-commit proof.

What was **not** verified, and must not be claimed as verified:

| Spike | Question | Status |
|---|---|---|
| S1 | Can a third-party operator make SourceHut **decline** a push from a pre-receive gate? | **UNVERIFIED.** The gate warns and never blocks. |
| S2 | Does Forgejo/Codeberg reject `push --mirror`, and does it delete the default branch? | Documented upstream; nuncio never generates mirror mode, so the failure cannot occur. Not live-tested. |
| S3 | Can Tangled create a repository through `org.tangled.repo.createRepo` before the first push? | Implemented to the documented XRPC shape; not live-tested. |
| S4 | Does GitLab's protected-branch API require unprotect → push → re-protect for a force-push? | Implemented with the numeric project ID and `/protected_branches` endpoints; not live-tested. |

To find out what *your* token can do, run:

```bash
nuncio doctor
```

It prints `api ok` or `api FAIL` plus the exact error, and never prints a token
value. A capability row that says `no` means the planner will refuse to emit
that action and will report it instead.

## Declared matrix

| Capability | GitHub | GitLab | Forgejo/Codeberg | SourceHut | Tangled |
|---|---|---|---|---|---|
| `git.push` | yes | yes | yes | yes | yes |
| `git.push_force` | yes | yes | yes | yes | yes |
| `git.ref_delete` | yes | yes | yes | yes | yes |
| `git.push_create_repo` | yes | yes | yes | no | no |
| `git.protected_branches` | yes | yes | yes | no | no |
| `git.lfs` | yes | yes | yes | no | no |
| `repo.create` | yes | yes | yes | yes | yes |
| `meta.read` / `meta.write` | yes | yes | yes | yes | yes |
| `event.webhook` | yes | yes | yes | yes | no |
| `event.firehose` | no | no | no | no | yes |
| `event.pre_receive_gate` | no | no | no | yes | no |
| `event.push_options` | yes | no | yes | no | no |
| Reserved namespaces | `refs/pull/*` | none declared | `refs/pull/*` | none declared | `refs/cobs/*`, `refs/cob-checkpoints/*`, `refs/hidden/*`, `refs/atproto/*` |

Reserved namespaces are never written. A ref that exists only inside a reserved
namespace is reported as `namespace` and skipped, not treated as missing.

## Authentication

Credentials are references, never literals. A config value such as
`env:GITHUB_TOKEN` is resolved at startup; a literal token in the `auth` key is
rejected by config validation.

| Scheme | Meaning |
|---|---|
| `env:NAME` | read the environment variable |
| `file:PATH` | read the first line of a file, `~` expanded |
| `ssh:NAME` | use `~/.ssh/NAME` as the identity; `IdentitiesOnly yes` is enforced |
| `keyring:SERVICE` | reserved; not implemented on all hosts |

For HTTPS git transport the token is placed in a per-invocation
`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_*` header, never in the command line and never
in the process environment as a URL. For SSH, `GIT_SSH_COMMAND` binds the named
key with `IdentitiesOnly yes`.

### GitHub

- API base: `https://api.github.com`
- API auth: `Authorization: Bearer <token>`, plus
  `Accept: application/vnd.github+json` and `X-GitHub-Api-Version: 2022-11-28`
- Classic token scopes: `repo`, `read:org` (for organisation repository lists)
- Fine-grained: Contents read/write, Metadata read, Administration read/write
  only if branch protection must be toggled
- Endpoints used: `GET /user`, `GET /user/repos`, `GET /repos/{owner}/{repo}`,
  `POST /user/repos` or `POST /orgs/{org}/repos`, `PATCH /repos/{owner}/{repo}`,
  `PUT /repos/{owner}/{repo}/topics`, `DELETE|PUT
  /repos/{owner}/{repo}/branches/{branch}/protection`
- Push transport: HTTPS by default; SSH when `auth = "ssh:..."`.

### GitLab

- API base: `https://gitlab.com/api/v4`
- API auth: `PRIVATE-TOKEN: <token>`
- Token scopes: `api` (or `read_api` plus `write_repository`)
- Project identity: paths are URL-escaped, but branch-protection endpoints
  resolve the numeric project ID first because GitLab does not accept `%2F` in
  that path position.
- Endpoints used: `GET /user`, `GET /projects?membership=true`,
  `GET /projects/{id}`, `POST /projects`, `PUT /projects/{id}`,
  `DELETE|POST /projects/{id}/protected_branches`
- Push transport: SSH by default; HTTPS when a token is configured.

### Forgejo / Codeberg

- API base: `https://codeberg.org/api/v1` (any Forgejo instance works)
- API auth: `Authorization: token <token>`
- Token scopes: `write:repository`, `write:user` for organisation listing
- Endpoints used: `GET /user`, `GET /user/repos`, `GET /repos/{owner}/{repo}`,
  `POST /user/repos` or `POST /orgs/{org}/repos`, `PATCH /repos/{owner}/{repo}`,
  `DELETE|POST /repos/{owner}/{repo}/branches/{branch}/protection`
- Push transport: SSH by default.

### SourceHut

- API base: `https://git.sr.ht/query` (GraphQL)
- API auth: `Authorization: Bearer <token>`
- Token scopes: `git.sr.ht/REPOSITORIES:RO`, `git.sr.ht/REPOSITORIES:RW`,
  `git.sr.ht/OBJECTS:RW` for pushes
- Endpoints used: `query { me { ... } }`, paginated
  `me.repositories(cursor:)`, `repository(name:, owner:)`,
  `createRepository(...)`, `updateRepository(...)`
- Push transport: SSH (`ssh://git@git.sr.ht/~user/repo`).
- Pre-receive gate: **not enforced.** The handler at `/gate/{owner}/{name}`
  warns with the peer tip and the inspection command, then allows the push. The
  warning is derived from nuncio's last observation, not from a live read.
- LFS is not available; a repository that uses LFS is refused on this platform
  with an explicit message rather than a silent corruption.

### Tangled

- API base: the knot host (`https://tangled.org` by default)
- API auth: `Authorization: Bearer <token>` for XRPC; SSH for git
- Endpoints used: `GET /xrpc/com.atproto.server.getSession`,
  `GET /xrpc/org.tangled.repo.listRepos`, `GET /xrpc/org.tangled.repo.getRepo`,
  `POST /xrpc/org.tangled.repo.createRepo`,
  `GET /xrpc/com.atproto.repo.getRecord`
- Push transport: SSH to the knot.
- Events: `wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos` (configurable).
  Frames are CBOR; only `#commit` frames whose `repo` DID belongs to a managed
  repository wake the scheduler. On `FutureCursor` or `ConsumerTooSlow` the
  cursor is dropped and every managed repository is woken, because replaying a
  hole would be a lie about freshness.
- Tangled reserves `refs/cobs/*`, `refs/cob-checkpoints/*`, `refs/hidden/*` and
  `refs/atproto/*`; these are skipped and reported.

### Plain git

`kind = "git"` is a first-class adapter used for local and test endpoints. It
has no API: `GetRepo` reports the path as existing, `CreateRepo` is unsupported,
and refs are read with `ls-remote`. It exercises the same planner, executor,
snapshots, verification and lost-commit proof as the hosted adapters. The test
suite builds every end-to-end scenario on two local bare repositories.

## Webhook signatures

| Platform | Header | Verification |
|---|---|---|
| GitHub | `X-Hub-Signature-256` | HMAC-SHA256 of the raw body, `sha256=` prefix, constant-time compare |
| Forgejo | `X-Forgejo-Signature` (or `X-Gitea-Signature`) | hex HMAC-SHA256, optional prefix |
| GitLab | `X-Gitlab-Token` | constant-time comparison with the shared secret |
| SourceHut | `X-Payload-Signature`, `X-Payload-Nonce` | ed25519 over `body + nonce`, base64 |

A delivery is accepted only if the platform is configured, the repository is
managed, the signature verifies, and the delivery ID (or body hash) has not been
seen before. The payload is then discarded and the scheduler is woken. Unsigned
deliveries are refused with 403 when no secret is configured; they are never
trusted by default. Retried deliveries receive 200 and do no work.

## Failed operations

Every mutation is followed by a read-back. A push that returns zero but leaves
the remote at a different SHA is a failure, not a success. Error classes:

- `Retryable` — exponential backoff up to `MaxAttempts`.
- `OperationalError` — auth, network, unreachable platform; exit 2.
- `SafetyError` — a destructive action without a preceding snapshot, or a
  snapshot that would overwrite a different SHA; exit 3.
