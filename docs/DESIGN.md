# Nuncio — Design Document

**Status:** handoff spec, v1.0
**Audience:** an implementing agent or engineer who has not read any prior discussion
**Reading time:** 25 minutes. Read §0–§4 before writing code. §12 is the delivery contract.

---

## 0. What this is, in one paragraph

Nuncio is a single-binary daemon plus CLI that keeps the same repository in sync across any
number of git forges — GitHub, GitLab, Forgejo/Codeberg, SourceHut and Tangled — treating **every
forge as a peer** instead of requiring one of them to be the hub. It never deletes a ref, never
deletes a repository, never silently picks a winner when two sides diverge, and produces a
per-ref reconciliation report that answers the only question a human asks after a sync incident:
*"what did I lose?"* — with a list of SHAs as proof, usually `0 commits`.

The name is the design thesis. A *nuncio* is a diplomatic envoy sent between two sovereign courts.
He carries binding communications, he does not hold authority of his own, and he never becomes
sovereign himself. Nuncio carries refs between your forges and refuses, by default, to become the
owner of your history.

**Tagline:** *A courier between sovereign forges. Never the owner of your history.*

---

## 1. Why the existing tools are not this

Seven tools already do cross-forge mirroring. This is not a crowded-but-unexamined space; it is a
crowded-and-mostly-solved space with three structural holes. Nuncio is aimed at the holes, not at
the space.

| Project | Bidirectional | Forges | Structural limitation |
|---|---|---|---|
| `plainskill/reposync` | yes | Forgejo ↔ GitHub only | Hardcoded to two forges, both REST-with-webhooks |
| `MaigoLabs/refray` | yes (∞-side) | GitHub, GitLab, Gitea, Forgejo | "Rebase or open a PR" as the conflict answer |
| `vaultdmn/gitspan` | mostly one-way | GitHub, GitLab, Bitbucket, Forgejo | Webhook → `git mirror` pipeline |
| `sgkens/hydrozoa`, `TIVerse/multigit`, `ihsanmokhlisse/copygit`, `ev-the-dev/gitgogit` | partial | the usual four | `warn`/`merge`/`force` conflict triple |
| `stepbrobd/miroir` | no | **includes SourceHut** | The TOML config *is* the truth; `sync` reconciles forges toward it |
| `FollowTheProcess/tangled-mirror` | no | GitHub → Tangled | A GitHub Action, one direction |
| `~xyhhx/push-mirror` | no | sr.ht → out | A build hook |

The three holes:

**Hole 1 — SourceHut is not modelled, it is papered over.** `miroir` pushes to `git.sr.ht` but
cannot read a divergence back from it in any structured way, because it treats every forge as a
`remote` with a URL and an API token. SourceHut is not that. It has no REST API. It has GraphQL
webhooks whose payload is *a query you write*, its repository creation takes a `cloneUrl` for
seeding, and — uniquely among these five — it exposes `GIT_PRE_RECEIVE`, a hook that fires **before
the push is accepted**. Nothing in the table above uses that. A sync daemon that can decline a push
*before it lands* is in a different risk class from one that cleans up afterwards.

**Hole 2 — Tangled is not modelled at all.** Tangled has no server-side hooks (`"There are no
server-side hooks"`, knot docs), so every tool built around webhooks is structurally blind to it.
What it does have is a `com.atproto.sync.subscribeRepos` firehose that the knot publishes commits
and `org.tangled.git.refs` records onto, an XRPC method `org.tangled.repo.createRepo` for creating
repositories, reserved ref namespaces (`refs/cobs/*`, `refs/cob-checkpoints/*`, `refs/hidden/*`,
`refs/atproto/*`) that a blind `push --mirror` will be rejected for, and per-repository records
living on the repository's *own* DID. The only existing Tangled tool is a one-way GitHub Action.

**Hole 3 — and this is the conceptual one — platform asymmetry is treated as a bug to abstract
away, not as a thing to model.** Every tool above converts forges into an abstract `remote` and
then spends its complexity budget on patches: `gitgogit` ships `exclude_refs: [refs/pull/*,
refs/merge-requests/*]` because GitHub's pull-request refs are read-only and Forgejo reserves the
namespace; `miroir` has a per-forge "archive via sync: yes/no" column; `reposync` documents that
organizations are ignored. Each of these is one platform's capability leaking through an
abstraction that denied it existed.

Nuncio's core idea is the inverse: **the capability matrix is a first-class object.** It is
declared per platform, it is validated in `nuncio doctor`, it appears in the config the user edits,
and it is printed in the report when it causes a ref to be skipped. Nothing is abstracted into
uniformity; the asymmetry is the input to the algorithm.

A fourth, smaller hole: **no tool publishes a machine-checkable statement that nothing was lost.**
They all log. None of them can be asked, in one command, *"prove that no commit became unreachable
because of what you did last night."* Nuncio's report is built to answer exactly that.

---

## 2. Non-goals

Stating these is part of the product. Each one is a scope decision, not a limitation of effort.

1. **v1 does not sync issues, pull requests, comments, or CI state.** Platform schemas are
   incompatible, and only one direction is even tractable (see §13.2). `miroir` and `reposync` both
   carve this out for the same reason.
2. **Nuncio is not a backup tool.** It is a synchroniser. A backup has a source of truth; Nuncio
   deliberately does not.
3. **Nuncio never deletes.** Refs, repositories, and objects are append-only from its point of
   view. A deletion on one forge is a *fact to be recorded and reported*, and propagating it is an
   opt-in policy that still leaves a snapshot behind.
4. **Nuncio does not host.** It has no web UI beyond a one-page status view, no auth system of its
   own, no multi-tenant story. One instance, one operator, N repositories.
5. **Nuncio does not resolve merge conflicts.** It preserves them, at a permanent ref, and tells
   you where they are. Automatic conflict resolution on a bidirectional sync is how history gets
   destroyed.

---

## 3. Design principles

These are invariants. An implementation that violates one is wrong even if it works.

**P1. Never mislead about what happened.**
Every push is verified by reading the remote back (`ls-remote` / API branch read) and comparing
SHAs. A push command that exited 0 is not evidence. This is not a nicety — three of the seven
tools above have open issues that are exactly "reported success, target was on the wrong branch".

**P2. Snapshot before overwrite, always.**
Any action that could make a SHA unreachable first writes that SHA to a snapshot ref on the same
remote. Even under `authority = "github"` with `on_divergence = "force"`. Even when the operator
passes `--force`. There is no code path that skips this.

**P3. Divergence is parked, not judged.**
Default behaviour on true divergence is to leave both tips exactly where they are, record the pair
of SHAs plus the merge base, and optionally write a merge attempt to a namespaced ref so a human
can inspect the conflicts. `reposync` arrived at this independently, which is corroboration that it
is the right answer.

**P4. Asymmetry is data.**
A platform's refusals, reserved namespaces, missing capabilities and rate limits are declared in
`capabilities`, checked before an action is planned, and cited in the report when they block one.

**P5. Every path is idempotent and resumable.**
Killing the process mid-reconcile must leave the system in a state a second run can finish from.
Per-repo advisory lock, per-action journal entry, no in-memory-only progress.

**P6. Convergence over time, not transactionality.**
Two forges cannot be updated atomically. Nuncio does not pretend to. It aims for a state where,
after a bounded number of reconciles, every ref either agrees or is recorded as a declared
divergence. It will frequently be mid-flight; that is fine and the report says so.

**P7. The operator's first run must be harmless.**
`nuncio plan` and `nuncio sync --dry-run` are the documented first commands. `init` writes a config
that is safe by construction (all `dry_run`, all park). There is no "quickstart that just works and
force-pushes".

---

## 4. Capability matrix

The matrix is the conceptual core. It exists as a Go type, a config block, a `doctor` check, and a
column in the report.

### 4.1 Dimensions

```
Capabilities {
    git.push                 bool     // can we push at all
    git.push_force           bool     // can we force-push a branch
    git.ref_delete           bool     // can we delete a ref
    git.push_create_repo     bool     // does push-to-unknown-path create the repo
    git.protected_branches   bool     // may force-push be rejected regardless of token
    git.lfs                  bool     // does the platform serve LFS objects
    repo.create              bool
    repo.delete              bool
    meta.read                bool     // description, visibility, topics, default branch
    meta.write               bool
    event.webhook            bool
    event.firehose           bool
    event.pre_receive_gate   bool     // can we influence acceptance before it lands
    event.push_options       bool     // -o key=value on push is interpreted
    issue.read               bool
    issue.write              bool
    reserved_refs            []glob   // namespaces the platform owns and rejects writes to
    auth.push                []enum   // ssh_key | https_pat | https_service_token
    auth.api                 enum     // pat | oauth2_bearer | none
    limits                   map      // per_file, per_repo, per_account, per_push, rate
}
```

### 4.2 Known-verified cells

Only cells confirmed against primary documentation are filled. A `?` is a task for the
implementer, not a value to guess — see §12 M0.

| Dimension | GitHub | GitLab | Forgejo / Codeberg | SourceHut (git.sr.ht) | Tangled (knot) |
|---|---|---|---|---|---|
| `git.push` | yes | yes | yes | yes (SSH) | yes (SSH; HTTPS needs minted token) |
| `git.push_force` | yes, minus protected refs | yes, minus protected refs | yes | yes | yes |
| `git.ref_delete` | yes | yes | yes | yes | yes, minus reserved namespaces |
| `git.push_create_repo` | yes (private-default is a Forgejo/GitLab trait; GitHub `?`) | yes, and **creates private** regardless of source visibility | `?` | `?` | no (must call `org.tangled.repo.createRepo`) |
| `git.protected_branches` | yes | yes, blocks force-push to protected `main` | `?` | `?` | n/a |
| `git.lfs` | yes | yes | yes | `?` | yes |
| `repo.create` | yes | yes | yes | yes, `createRepository(name, visibility, description, cloneUrl)` | yes, XRPC `org.tangled.repo.createRepo` |
| `repo.delete` | yes | yes | yes | yes | `?` |
| `meta.read` | yes | yes | yes | yes | yes (repo DID records) |
| `meta.write` | yes | yes | yes | yes, `updateRepository(RepoInput)`, `HEAD` sets default branch | yes (`com.atproto.repo.putRecord` on the manifest) |
| `event.webhook` | yes, HMAC-signed | yes | yes | yes, GraphQL-native, payload is a query you write; `X-Payload-Signature` ed25519 + `X-Payload-Nonce` | **no** |
| `event.firehose` | no | no | no | no | yes, `com.atproto.sync.subscribeRepos` |
| `event.pre_receive_gate` | no | no | no | **yes** (`GIT_PRE_RECEIVE` event exists — see Spike S1) | no |
| `event.push_options` | partial | `?` | `?` | `?` | yes (`-o skip-ci`, `-o verbose-ci`, `source=`) |
| `issue.read` | yes | yes | yes | no native issues | yes, COB records on the repo DID |
| `issue.write` | yes | yes | yes | n/a | yes |
| `reserved_refs` | `refs/pull/*` (read-only) | `refs/merge-requests/*` | `refs/pull/*` | `?` | `refs/cobs/*`, `refs/cob-checkpoints/*`, `refs/hidden/*`, `refs/atproto/*` |
| `auth.push` | ssh_key, https_pat | ssh_key, https_pat | ssh_key, https_pat | ssh_key | ssh_key, https_service_token (`org.tangled.git.receivePack`) |

### 4.3 Why the matrix is load-bearing

Three concrete behaviours fall out of the table and would be impossible to get right from a uniform
`remote` abstraction:

1. Pushing `refs/pull/*` from GitHub to any Forgejo target is **rejected**, and `push --mirror`
   additionally attempts to delete the default branch and is **rejected** for that reason. A planner
   that reads `reserved_refs` never generates those commands.
2. Force-pushing to GitLab `main` fails on protected branches. With `git.protected_branches = true`
   the planner emits an unprotect → push → re-protect sequence inside one journalled action group,
   so a crash cannot leave `main` unprotected.
3. Tangled cannot be created by push-to-unknown-path. `repo.create` being false for the adapter
   means `nuncio` must call the XRPC first and then push, and the report must distinguish "created"
   from "pushed" because the failure modes differ.

---

## 5. Architecture

```
                        ┌──────────────────────────────────┐
   git push ───────────▶│  event sources                    │
   webhook POST ───────▶│  · webhook receiver (HTTP)        │
   firehose frame ─────▶│  · Tangled firehose subscriber    │
   timer tick ─────────▶ │  · pre-receive gate (sr.ht)       │
                        │  · reconcile timer (all)          │
                        └───────────────┬──────────────────┘
                                        │ all sources converge
                                        ▼
                        ┌──────────────────────────────────┐
                        │  Scheduler                        │
                        │  per-repo token bucket, lock,     │
                        │  coalesce duplicate wakes          │
                        └───────────────┬──────────────────┘
                                        ▼
        ┌────────────────────────────────────────────────────────────┐
        │  Reconciler (pure)                                          │
        │  plan(observed, policy, capabilities) -> []Action           │
        └───────────────┬────────────────────────────────────────────┘
             ┌──────────┴───────────┐
             ▼                      ▼
     ┌───────────────┐      ┌──────────────────┐
     │ Object cache  │      │  Executor         │
     │ bare git per  │◀────▶│  journal, retry,  │
     │ repo, refs/   │      │  snapshot refs,   │
     │ nuncio/...    │      │  read-back verify │
     └───────────────┘      └────────┬─────────┘
                                     ▼
                            ┌──────────────────┐
                            │ State (SQLite)   │
                            │ refs, pairs,     │
                            │ journal, reports │
                            └──────────────────┘
```

### 5.1 One reconcile path

Every event source does exactly one thing: it decides that repository *R* is worth reconciling and
wakes the scheduler. It never acts. A webhook arriving is a hint that something changed, not a
description of what changed — platforms disagree too much about payload shape and delivery
guarantees for a payload to be trusted. `reposync` documents the same conclusion ("webhooks only
wake that path; a 5-minute timer is the same path"), and it is correct for a reason that matters:
a sync daemon that trusts webhook payloads will corrupt state the first time a delivery is
misordered, duplicated, or lost.

### 5.2 Object cache

One bare repository per logical repo at `<state>/repos/<slug>.git`, containing the union of objects
seen on all platforms. Fetches are per-platform into private namespaces:

```
refs/nuncio/observed/<platform>/<refname>   # last fetched tip per ref per platform
refs/nuncio/snapshot/<refname>/<unix-ts>    # local mirror of snapshots we wrote
```

The cache exists for one reason: **ancestry tests are local**. Deciding whether a divergence is a
fast-forward, a rewrite, or a real divergence needs `git merge-base --is-ancestor`, which needs
objects. Doing that by transferring objects on every check is what makes naive mirror tools
unusable on large repos. `ls-remote` gives the cheap observed state; only refs whose SHAs changed
get fetched.

### 5.3 Executor

Actions execute in a journalled group per repo. Each group records intended actions, then executes,
then records outcomes, then verifies. Idempotence rule: an action's identity is
`(repo, class, ref, from_sha, to_sha)`, so replaying a journalled group is safe.

Retry policy is per action, not per run, with jittered backoff. A failed verification is **never**
retried as a force push — that is the one retry that can destroy data, and it is forbidden at the
type level (a `VerifyFailed` result cannot be fed back into an `Action` whose class is not
`Noop` or `Report`).

### 5.4 Why single binary

`go install` is one line. A static `linux/amd64` binary drops onto any VPS with no runtime, no
interpreter, no Python venv, no Node. The whole dependency surface is: the Go standard library, one
SQLite driver, `git`. Nothing else. No Redis, no Postgres, no message queue, no reverse proxy
required. This is a hard requirement from the delivery contract (§12), not a preference — the tool
will be linked from X and people will run it in five minutes or not at all.

---

## 6. Configuration

TOML. One file, `nuncio.toml`, in the repository you are operating from or pointed at by `-c`.
Secrets are never in this file — only references to them (`env:NAME`, `file:/path`, `ssh:alias`,
`keyring:`).

```toml
# nuncio.toml
version = 1

[state]
path = "/var/lib/nuncio/state.db"
cache = "/var/lib/nuncio/repos"

[daemon]
listen = "127.0.0.1:8787"   # empty string disables the webhook listener entirely
interval = "15m"            # reconcile timer floor, per repo
concurrency = 4
dry_run = true              # safe by construction on init

[identity]
# author used only for commits nuncio itself creates (merge attempts)
name = "nuncio"
email = "nuncio@localhost"

# ---------------------------------------------------------------- platforms
[platforms.github]
kind = "github"
api  = "https://api.github.com"
git  = "https://github.com"
auth = "env:GITHUB_TOKEN"

[platforms.gitlab]
kind = "gitlab"
api  = "https://gitlab.com/api/v4"
git  = "ssh://git@gitlab.com"
auth = "env:GITLAB_TOKEN"

[platforms.codeberg]
kind = "forgejo"
api  = "https://codeberg.org/api/v1"
git  = "ssh://git@codeberg.org"
auth = "ssh:codeberg"

[platforms.srht]
kind = "sourcehut"
api  = "https://git.sr.ht/query"
git  = "ssh://git@git.sr.ht"
auth = "env:SRHT_TOKEN"
pre_receive_gate = true      # install GIT_PRE_RECEIVE hooks on managed repos

[platforms.tangled]
kind     = "tangled"
knot     = "tangled.org"
owner    = "alice.tngl.sh"
git      = "ssh://git@tangled.org"
auth     = "ssh:tangled"
firehose = "wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos"

# ---------------------------------------------------------------- policies
[policy.default]
platforms       = ["github", "codeberg"]
authority       = "none"       # none | <platform-name>
on_divergence   = "park"       # park | merge | force-from-authority
on_delete       = "ignore"     # ignore | propagate
snapshot        = true
verify          = true

[policy.site]                  # policy blocks are reusable
platforms       = ["github", "gitlab", "codeberg", "srht"]
default_branch  = "main"
authority       = "none"

[policy.paper]
platforms       = ["github", "gitlab"]      # source of truth: him, dual warm copy
authority       = "github"
on_divergence   = "park"                    # authority is only used when told to force
```

Repositories, and groups that generate them:

```toml
[repos."alice/example"]
policy    = "default"
platforms = ["github", "codeberg", "tangled"]

[repos."alice/example".refs."refs/heads/main"]
authority = "github"                    # main is authored on GitHub, others follow

[repos."alice/example".refs."refs/tags/*"]
on_divergence = "park"
on_delete     = "propagate"             # a deleted tag still leaves a snapshot

[repos."alice/site"]
policy    = "site"
platforms = ["github", "gitlab", "codeberg"]

[groups.papers]
policy = "paper"
select = { topic = "paper", platform = "github" }   # resolved by `nuncio discover`
```

Rules:

- `platforms` on a repo must all exist in `[platforms.*]`.
- `authority` must name one of the repo's platforms, or be `"none"`.
- `refs` blocks are globs, matched most-specific-first (`refs/heads/main` beats `refs/heads/*`
  beats `refs/tags/*` beats the policy default).
- `on_divergence = "force-from-authority"` requires `authority != "none"`. Validation error
  otherwise. This pairing is deliberate: forcing is only expressible when a human has named a
  sovereign for that ref.
- `select` in a group is resolved at reconcile time by querying each platform's repository list; a
  repo that disappears from the selector is *not* removed from management, only reported as
  "selector no longer matches".

---

## 7. Reconciliation semantics

### 7.1 Ref classification

For each ref present on at least one platform in scope:

| Class | Detection | Default action |
|---|---|---|
| `equal` | all platforms report the same SHA | none |
| `fast-forward` | new tip descends from old tip on the lagging side | push to lagging side |
| `history-rewrite` | tips are unrelated but one side's history is a strict rewrite of the other (shared subjects, disjoint SHAs) | snapshot the loser, then follow `on_divergence` |
| `divergence` | `merge-base --is-ancestor` false in both directions | `on_divergence` |
| `created` | ref present on one platform only, no prior observation | create on the others |
| `deleted` | ref observed before, absent now on one platform | `on_delete` |
| `tag-moved` | annotated tag object changed | treat as `history-rewrite` |
| `namespace-rejected` | ref matches target platform's `reserved_refs` | skip, record, report |
| `capability-blocked` | required capability false | skip, record, report |
| `unreachable` | platform errored / auth failed | retry, then record as `unknown` — **never** interpreted as `deleted` |

The `unreachable` case is the single most dangerous misclassification in this whole design. A
platform being down must never look like a deletion. Detection requires a successful listing; an
error yields `unknown` and blocks any destructive inference for that ref.

### 7.2 Actions

| Action | Effect |
|---|---|
| `Push{ref, to, from_sha, into_sha}` | `git push <target> <from>:<ref>` |
| `ForcePush{ref, to, sha}` | snapshot target tip, then `git push --force-with-lease=<ref>:<observed>` |
| `Snapshot{platform, sha, tag}` | `git push <platform> <sha>:refs/nuncio/snapshot/<ref>/<ts>` |
| `MergeAttempt{ref, base, ours, theirs}` | create merge commit on `refs/nuncio/merge/<ref>/<ts>`, push to every platform |
| `Unprotect{platform, branch}` / `Reprotect{...}` | platform-specific, always paired, always in one group |
| `CreateRepo{platform, ...}` | platform-specific create; `source` seeding where supported |
| `SetMeta{platform, field, value}` | description, visibility, topics, default branch |
| `Report{...}` | writes a finding into the report with no remote mutation |

Every `ForcePush` is preceded in the same group by a `Snapshot` of the tip it will replace.

### 7.3 Per-ref authority

`authority = "github"` means: on divergence, GitHub's tip is the reference, and the other side is
converged toward it — **but only if** `on_divergence = "force-from-authority"`. With
`on_divergence = "park"` (the default even when an authority is named), an authority produces a
different report line, not a different action. Naming an authority is a declaration of intent,
not a licence to overwrite. This distinction is the same one P3 makes and it is worth an explicit
sentence in the README, because users will assume the opposite.

### 7.4 Multi-platform divergence (n > 2)

With three or more peers, classification generalises to a *set* of distinct SHAs per ref. Cases:

- 1 distinct SHA → `equal`.
- 2 distinct, one ancestor of the other → `fast-forward`.
- 2 distinct, unrelated → `divergence`.
- ≥3 distinct → `divergence` with a `peers` list. Nuncio never resolves an n-way divergence; it
  parks it and reports the full set. Attempting to reduce n-way to pairwise creates an ordering
  dependency that makes the outcome non-deterministic across runs, which violates P6.

### 7.5 Deletion and rename

`on_delete = "propagate"` still writes `Snapshot` on the remote that is about to lose the ref before
deleting it, and records the pair `(ref, last_sha)` in the report forever. Repository deletion is
never propagated, ever, under any policy. Renames are not tracked at all in v1 — a renamed
repository appears as one repo deleted and one created, both of which are refused or reported, and
the report says so.

---

## 8. Event sources

All four converge on the scheduler. Each is optional; with none configured, nuncio is a pure timer
daemon and needs no public address. That configuration must be fully supported and documented,
because "I have to expose a public webhook endpoint" is the single biggest reason people do not
self-host a sync daemon.

### 8.1 Webhook receiver

`POST /hooks/<platform>/<repo-slug>` on `daemon.listen`. Verifies the platform's signature
(GitHub HMAC-SHA256 in `X-Hub-Signature-256`, sr.ht ed25519 over body+nonce, Forgejo/GitLab their
own schemes), rejects unsigned or stale deliveries, then **discards the payload** and wakes the
scheduler. A 200 means "received", never "acted on".

`X-Payload-Nonce` and a delivery-id set in SQLite make replay harmless.

### 8.2 Tangled firehose

Long-lived WebSocket to the knot's `subscribeRepos`. Filters for `#commit` frames whose repo DID is
in the managed set, and for `org.tangled.git.refs` record ops. Reconnect with cursor; on cursor
loss, fall back to a timer reconcile. No signature to verify — the firehose is the knot's own
broadcast — but the managed-DID filter is what keeps this from doing work for strangers' repos.

### 8.3 SourceHut pre-receive gate

The interesting one. `git.sr.ht` exposes a webhook event `GIT_PRE_RECEIVE` that fires *before the
push is accepted*, and the GraphQL API has both `createGitWebhook(config: GitWebhookInput)` and a
`deliverGitHook(input: GitEventInput)` mutation documented as *"If it returns false, the push event
should be rejected. Invoked by git.sr.ht-update-hook."*

**This must be spiked before it is designed around — see Spike S1.** The open question is precisely
which mechanism a third-party operator can use to cause the rejection. The mutation note suggests
the rejecting decision belongs to `git.sr.ht-update-hook` calling back into git.sr.ht, not to an
HTTP endpoint returning a status. If the only operator-available lever turns out to be a
synchronous legacy webhook whose response text is printed to the pusher (which the `RepoWebhook.sync`
field in `git.sr.ht`'s source describes), then the feature degrades from *gate* to *loud warning*,
and the design must say so in the README rather than promise enforcement.

Design intent, contingent on S1: when a push to a managed sr.ht repo would create a divergence
class that the policy cannot resolve automatically, the gate refuses it and prints a sentence
naming the conflicting ref, the peer platform holding the other tip, and the exact command to
fetch it. This is materially better than reconciling after the fact, because the pusher still has
the context in their head. Contingent-on-S1 is stated because overpromising here would be a lie in
the README.

---

## 9. State model

SQLite, one file. WAL mode. Schema versioned with a `schema_version` table and forward-only
migrations.

```sql
platform     (id, name, kind, endpoint)
repo         (id, slug, policy, created_at, disabled)
repo_platform(repo_id, platform, remote_owner, remote_name, created_by_nuncio)
ref_state    (repo_id, refname, platform, sha, observed_at, first_seen_at, last_action)
pair         (repo_id, ref_a, sha_a, ref_b, sha_b, class, detected_at, resolved_at)
journal      (id, repo_id, group_id, action, refname, from_sha, to_sha, status,
              started_at, finished_at, error, attempts)
snapshot     (repo_id, platform, refname, sha, tag_name, created_at)
report_run   (id, repo_id, started_at, finished_at, dry_run, lost_commits, json)
webhook_seen (delivery_id, seen_at)
```

`ref_state` is the memory of what was true, and it is what makes `deleted` distinguishable from
`never existed` and both distinguishable from `unreachable`. Nothing is ever removed from
`ref_state`; a ref that disappears keeps its row with a cleared `sha` and the report can still name
its last known tip.

---

## 10. Reporting

The report is the product. Two renderings, one JSON schema.

Human rendering:

```
$ nuncio report --repo alice/example

alice/example
  github:alice/example   tangled:alice.tngl.sh/example   codeberg:alice/example

  REF                       CLASS            NOTICE
  refs/heads/main           fast-forward     github 9d3e0b1 -> tangled, codeberg
  refs/heads/feat/http2     divergence       parked. github 1a2b3c4 / tangled 5d6e7f8, base 0a0b0c0
  refs/tags/v1.2.0          equal            codeberg, github, tangled all 7f8e9d0
  refs/pull/12/head         namespace        github-only namespace, skipped by design

  lost commits        0
  snapshots written   refs/nuncio/snapshot/feat/http2/1759000000  (github, tangled)
  merge attempts      refs/nuncio/merge/feat/http2/1759000001    (conflicts preserved)

  to resolve:
    git fetch github refs/nuncio/snapshot/feat/http2/1759000000
    git fetch tangled refs/nuncio/snapshot/feat/http2/1759000000
    git log --oneline --graph 0a0b0c0..github 0a0b0c0..tangled
```

`lost commits` is computed, not declared: the count of commits reachable from any SHA observed at
any time that are reachable from no currently-existing ref on any platform and from no snapshot
ref. It should always be 0. If it is not, that is an incident and the report leads with it in the
first line rather than burying it.

JSON schema (`report --json`) is stable across versions and is what CI and cron should consume. It
carries, per repo, per ref: class, per-platform observed SHA, action taken, action result, and the
capability that blocked it if any.

---

## 11. CLI surface

```
nuncio init [--forge github,codeberg]   # wizard: detect remotes, write safe config
nuncio doctor                           # verify tokens, keys, endpoints, capabilities
nuncio discover [--platform P]          # populate [repos.*] from a forge's repo list
nuncio plan [--repo R]                  # compute actions, execute nothing
nuncio sync [--repo R] [--dry-run]      # reconcile once and exit
nuncio serve                            # daemon: timer + webhooks + firehose
nuncio report [--repo R] [--json] [--since DUR]
nuncio lost [--repo R]                  # the lost-commits computation, on its own
nuncio snapshot list|create|restore
nuncio doctor --print-systemd
nuncio doctor --print-docker
nuncio version
```

Exit codes are contractual, because people will put this in cron and CI:
`0` clean, `1` reconciliation completed with findings (divergence parked, refs skipped),
`2` operational error (auth, network, unreachable platform), `3` safety refusal (invariant would
be violated, or `--dry-run` blocked a destructive action). `1` must not be treated as failure by a
caller — it is "there is something for you to look at", which is the normal steady state for a
multi-master system.

`nuncio doctor` output is the capability matrix for *this operator's tokens*, not the generic one:

```
github    api ok   push ok   force ok   ref-delete ok   webhook ok   reserved refs/pull/*
gitlab    api ok   push ok   force ok   ref-delete ok   webhook ok   protected: main
codeberg  api ok   push ok   force ok   ref-delete ok   webhook ok
srht      api ok   push ok   force ok   ref-delete ok   gate UNVERIFIED (see S1)
tangled   api ok   push ok   force ok   ref-delete ok   firehose ok  reserved refs/cobs/* ...
```

---

## 12. Delivery contract

This section is what an implementing agent is judged on.

### 12.1 Repository layout

```
cmd/nuncio/main.go
internal/config/        TOML schema, validation, defaults, migration
internal/caps/          capability matrix types + per-forge declarations
internal/forge/         adapter interface and one package per forge
    github/ gitlab/ forgejo/ sourcehut/ tangled/
internal/gitx/          git command wrapper: ls-remote, fetch, push, lfs, merge-base
internal/cache/         bare object cache
internal/state/         SQLite store, migrations
internal/reconcile/     PURE: classify + plan. No I/O. This is where the tests live.
internal/execute/       journalled executor, snapshot, verify, retry
internal/events/        webhook receiver, firehose, gate, timer
internal/report/        human + JSON renderers, lost-commits computation
internal/fakeforge/     hostile fakes for the matrix test
docs/                   DESIGN.md (this file), ADAPTERS.md, DEPLOY.md
deploy/                 nuncio.service, docker-compose.yml, Dockerfile
examples/               nuncio.toml for 2, 3 and 5 forge setups
```

### 12.2 Hard requirements

1. **No global state, no singletons in `reconcile`.** `plan()` takes observed state, policy and
   capabilities and returns actions. No clock, no network, no filesystem. It must be testable with
   table-driven tests containing zero mocks.
2. **Every push verified.** `execute` reads the remote back and compares. A mismatch is an
   `OperationalError`, never a warning, never a retry-as-force.
3. **Snapshot before any overwrite, with no bypass flag.** `--no-snapshot` must not exist.
4. **`dry_run` defaults to true in the config that `init` writes**, and `init` prints the exact
   command to turn it off.
5. **One binary, static, Go 1.23+.** Dependencies allowed: a SQLite driver, a TOML parser, a
   WebSocket client. Nothing else. No CLI framework; use `flag` and subcommands.
6. **Windows-clean.** This author's primary machine is Windows with git-bash; paths handed to
   `git.exe` must be native (`D:/...`), never MSYS (`/d/...`). This is a known, documented trap and
   an adapter that gets it wrong silently writes to the wrong drive.
7. **LFS handled or refused loudly.** `git lfs fetch --all` before a push and `git lfs push --all`
   after, or the mirror holds pointers to objects that exist on one forge only. If LFS support is
   not implemented, the tool must *detect* `.gitattributes` LFS filters and refuse to claim a repo
   is synced.

### 12.3 Testing strategy — the hostile matrix

Not optional. The reason mirroring tools fail on real forges is that they are developed against one
friendly fake and then hit reserved namespaces, protected branches, and rejected mirrors.

`internal/fakeforge` implements the adapter interface with a configurable hostile profile:

```
reject_ref_delete      bool
reject_force_push      bool
reserved               []glob
create_on_push         bool      // does push-to-unknown-path create the repo
protected              []refglob
push_fails_every_nth   int       // flaky uplink simulation
event_source           none|webhook|firehose|gate
list_error_after_n     int       // simulates an outage mid-run
```

The matrix test runs the reconciler against all combinations that are semantically meaningful
(≈40 cells) and asserts, for each:

- no commit observed at any point becomes unreachable (the `lost == 0` invariant),
- no action is emitted whose required capability is false,
- no `deleted` is inferred from a `list_error`,
- a kill at any journalled step is recoverable by a second run.

Plus a `git config` golden-file test asserting no generated command line contains an MSYS path.

### 12.4 Milestones and acceptance

**M0 — spikes (verify, do not build).**
- S1: determine the operator-available mechanism for sr.ht `GIT_PRE_RECEIVE` rejection. Record the
  finding in `docs/ADAPTERS.md` with the exact request/response that proves it, or the proof that
  no operator-level rejection exists.
- S2: confirm Tangled HTTPS push by minting an `org.tangled.git.receivePack` service-auth token from
  a script; confirm `org.tangled.repo.createRepo` from a script.
- S3: confirm Forgejo `refs/pull/*` rejection and `push --mirror` default-branch rejection against
  Codeberg with a throwaway repo.
- S4: confirm GitLab force-push-to-protected-`main` failure mode and the unprotect round trip.
- Acceptance: each spike has a transcript in `docs/ADAPTERS.md` and a filled cell in the §4.2
  matrix. **No `?` may survive into M1.**

**M1 — skeleton.** Config parse + validate, capability types, `doctor` against real tokens,
`plan` that only lists (read-only, `ls-remote` only). Acceptance: `plan` on a live 3-forge repo
prints a correct classification table and mutates nothing.

**M2 — cache and observed state.** Bare cache, per-platform observed refs, SQLite, `report` with
`lost` computation. Acceptance: report on a repo with a known divergence names both SHAs and the
correct merge base.

**M3 — fast-forward only.** Execute `Push`, `Snapshot`, verify. Acceptance: matrix test green for
all fakeforge profiles with force and delete disabled; a real sync of a small repo across two
forges.

**M4 — divergence.** `park`, `merge`, `force-from-authority` with snapshot. Acceptance: an
artificially diverged repo is parked with snapshots on both sides and `lost == 0`.

**M5 — events.** Webhook receiver with signature verification and replay protection, Tangled
firehose subscriber, sr.ht gate per S1. Acceptance: a push to any of the five forges triggers
exactly one reconcile for that repo within 10s (webhook/firehose) and the daemon does no work for
unmanaged repos.

**M6 — multi-repo.** Groups, selectors, `discover`, per-repo concurrency and locking.
Acceptance: 50 repositories, `concurrency = 4`, a kill mid-run, restart, no duplicate actions, no
lost work. This is the deployment story and it must be demonstrated at scale, not asserted.

**M7 — packaging.** Static binaries for linux/amd64, linux/arm64, darwin/arm64, windows/amd64,
plus a multi-arch image. `deploy/` units and compose file. `examples/` for 2/3/5 forge topologies.
Acceptance: a stranger with a fresh VPS reaches a working `report` in under five minutes following
only `docs/DEPLOY.md`, with no public address and no webhook configured.

**M8 — optional, v1.5.** Read-only issue and metadata aggregation (`issue.read` columns only, no
writes). Do not start this before M7 ships.

---

## 13. Deployment

### 13.1 The five-minute path

```bash
# 1. install (any of these)
curl -fsSL https://github.com/NoahIsARider/nuncio/releases/latest/download/install.sh | sh
go install github.com/NoahIsARider/nuncio/cmd/nuncio@latest
docker run --rm ghcr.io/noahisarider/nuncio:latest nuncio version

# 2. from inside an existing clone
cd my-project
nuncio init            # detects forges from `git remote -v`, writes nuncio.toml (dry_run = true)
nuncio doctor          # tells you which tokens are missing, by name, not value
nuncio plan            # what would happen. changes nothing.

# 3. when the plan looks right
nuncio sync            # one shot
nuncio serve           # or: leave it running
```

`init` is allowed to fail informatively: if `git remote -v` shows only one forge, it says so and
prints the `[platforms.*]` block to add for each additional forge the user names.

### 13.2 Deployment shapes

| Shape | Public address | Event sources | Use |
|---|---|---|---|
| Timer only | none | timer | laptop, NAS, any VPS behind NAT. **Default.** |
| Timer + webhooks | one HTTPS endpoint | webhook receiver | VPS with a domain |
| Timer + firehose | outbound only | Tangled firehose | VPS without ingress needed |
| Full | HTTPS endpoint + outbound | all | maximum freshness |

The compose file documents all four with comments. The systemd unit is `Restart=always`,
`EnvironmentFile=/etc/nuncio/env` (mode 0600), `StateDirectory=nuncio`.

### 13.3 Token minimum scopes

Documented per platform, because over-scoping is how a sync tool becomes a liability.

| Platform | Minimum | Notes |
|---|---|---|
| GitHub | `contents: read+write`, `metadata: read`, `administration: read+write` only if repo creation is used | fine-grained PAT, per-repo |
| GitLab | `api` scoped to the groups in play | project access tokens preferred |
| Forgejo/Codeberg | repo read+write | basic auth from credential helper is acceptable |
| SourceHut | `REPOSITORIES` (rw), `OBJECTS`, `PROFILE` (ro) | GraphQL PAT from meta.sr.ht |
| Tangled | SSH key in the account; service-auth for HTTP push | no PAT system |

---

## 14. Security

- Secrets never in `nuncio.toml`. References only. `doctor` prints names, never values. A config
  whose `auth` field contains something that looks like a literal token is a validation error, not
  a warning.
- Webhook endpoints verify signature before parsing; unsigned deliveries are 403 and logged with
  delivery id only.
- The state DB contains remote URLs, ref names and SHAs. Not secrets. Documented as such so people
  are comfortable backing it up.
- `nuncio serve` binds `127.0.0.1` by default. Binding `0.0.0.0` requires an explicit flag and
  prints a warning naming the missing TLS terminator.
- No telemetry, no phone-home, no update check. Stated in the README because self-hosting audiences
  ask.

---

## 15. Launch notes (X)

The project will be announced on X, so the README must do these things and nothing else.

1. **The first screen is the report.** A screenshot-equivalent code block of the `nuncio report`
   output above. It communicates "explainable" in one glance, and no competitor has it.
2. **Say the non-goals early.** "Does not sync issues. Does not delete anything. Does not pick a
   winner." Honesty is the differentiator in a space with seven half-working tools.
3. **Say what is not verified.** If S1 concludes sr.ht cannot gate, the README says "sr.ht pushes
   are warned about, not blocked", not "sr.ht is protected". There is no version of this project
   that survives overclaiming.
4. **One command, one screenshot, one link to DEPLOY.md.**

Suggested thread skeleton (author writes the final copy):

> 1. I keep the same repos on five forges. Every mirroring tool I tried assumed one of them was the
>    boss. So I wrote one that doesn't. It's called Nuncio — a nuncio is an envoy between two
>    sovereign courts who never becomes one.
> 2. The thing nobody else does: when two forges disagree, Nuncio refuses to decide. It parks both
>    tips, snapshots them to permanent refs, and reports the merge base. Then it tells you how many
>    commits were lost. The number is 0, and it proves it.
> 3. It models what each forge actually can do — GitHub's read-only `refs/pull/*`, Forgejo's refusal
>    of `push --mirror`, GitLab's protected branches, Tangled's reserved `refs/atproto/*`, SourceHut's
>    pre-receive hook — instead of pretending they're all the same `remote`.
> 4. Single static binary, SQLite, works with no public address and no webhooks if you don't want
>    them. `nuncio init`, `nuncio doctor`, `nuncio plan`, done.
> 5. Source is on Tangled. Mirror on GitHub. `nuncio` also syncs *to* SourceHut, which
>    is where this whole thing started.

---

## 16. Appendix — adapter notes and open questions

**GitHub.** `refs/pull/*` is read-only for users and must be in `reserved_refs`. Fine-grained PATs
are per-repository, so `discover` over a whole account needs a classic token or an org-level app.
Push-to-unknown-path creating a private repo is documented for GitLab and Forgejo; confirm for
GitHub in S1-era spikes.

**GitLab.** Push-to-create lands the project **private** regardless of source visibility — the
adapter must follow a create with an explicit visibility `PUT` and read it back, or a public repo
silently becomes private and nobody notices. Force-push to protected `main` is refused; the
unprotect → push → re-protect sequence must be one journalled group. GitLab pages identity
verification is irrelevant here but is the same account-level trap in other tasks.

**Forgejo / Codeberg.** Reserves `refs/pull/*` for its own PRs. `push --mirror` is rejected because
mirror mode deletes remote refs the clone lacks, including the default branch. Account-wide storage
quotas mean a sync can fail for reasons unrelated to this tool; the adapter should surface the
platform's error text rather than a generic push failure.

**SourceHut.** No REST API. Everything is GraphQL at `git.sr.ht/query` with a PAT from
`meta.sr.ht/oauth2`. `createRepository(name, visibility, description, cloneUrl)` can seed from a
remote, which is the cheapest way to bootstrap a new sr.ht target. `updateRepository(RepoInput)`
sets `HEAD` (default branch), `name`, `description`, `visibility`, `readme`. Webhook payloads are
GraphQL queries you author, evaluated read-only in webhook context, signed with ed25519 over body +
`X-Payload-Nonce`, public key from `<service>/query/api-meta.json`. Webhook queries cannot mutate.
Open: S1.

**Tangled.** A repository is eight or so ATProto records on its own DID, hosted on a knot. No
server-side hooks. Firehose at `subscribeRepos` carries `#commit`, `#identity`, `#account` frames
plus `org.tangled.git.refs`. Reserved ref namespaces `refs/cobs/*`, `refs/cob-checkpoints/*`,
`refs/hidden/*`, `refs/atproto/*`; `refs/cobs/*` is fetchable read-only with an explicit prefix.
SSH push authenticates against `org.tangled.key.sshKey` records; set `IdentitiesOnly yes` for the
knot host or the server gives up before trying the right key. HTTPS push needs an atproto
service-auth token bound to `org.tangled.git.receivePack`. `org.tangled.repo.createRepo` accepts
`name`, `did`, `defaultBranch`, `spindle`, `description`, `website`, `topics`, `source` (for
import/fork). Repositories cannot currently migrate between knots. Issues and pulls are COB records
on the repo DID — the only forge here whose issue data is an open schema, which is why issue sync
is tractable from Tangled outward and not inward.

**Cross-platform.** Commit SHAs are preserved across forges for ordinary mirrored commits, because
objects are byte-identical. Commits nuncio itself creates (merge attempts) get `[identity]` as
author and must never be pushed as a repair to a user branch without being on a namespaced ref.

---

## 17. Open questions the implementer must answer

1. S1 — sr.ht pre-receive rejection mechanism for a third-party operator. Blocks §8.3's wording and
   the launch thread's claim 3.
2. Does any of the five forges reject LFS pushes over a size or count threshold in a way that
   surfaces as a successful push? (The Forgejo/Codeberg quota behaviour suggests it can.)
3. What is the sane `interval` default for a repo > 1 GB? The cache fetch cost, not the ref
   comparison, dominates.
4. Does the Tangled firehose delivery ordering guarantee anything about `org.tangled.git.refs`
   versus `#commit` for the same push? If not, the firehose path must debounce both into one wake.
5. Should `merge` mode's merge commit be authored by nuncio or by the operator's configured
   identity on that platform? Default to nuncio, but this is a user-facing decision.
