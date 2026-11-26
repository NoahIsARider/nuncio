# nuncio

**A courier between sovereign forges. Never the owner of your history.**
[![ci](https://github.com/NoahIsARider/nuncio/actions/workflows/ci.yml/badge.svg)](https://github.com/NoahIsARider/nuncio/actions/workflows/ci.yml)

`nuncio` keeps one repository in sync across GitHub, GitLab, Codeberg, SourceHut and Tangled,
treating every forge as a peer. It does not elect a hub. It does not delete refs. It does not
decide who wins when two sides diverge — it parks the divergence, snapshots both tips to permanent
refs, and then proves how many commits were lost.

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

That `lost commits 0` line is the whole point. Seven mirroring tools exist. None of them can be
asked, in one command, *"prove that nothing became unreachable because of what you did last night."*

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/NoahIsARider/nuncio/main/install.sh | sh
# or
go install github.com/NoahIsARider/nuncio/cmd/nuncio@latest
# or
docker run --rm ghcr.io/noahisarider/nuncio:latest nuncio version
```

Static binary. SQLite. No runtime, no Postgres, no Redis, no message queue.

## Five minutes, from inside an existing clone

```bash
cd my-project
nuncio init        # reads `git remote -v`, writes nuncio.toml with dry_run = true
nuncio doctor      # names the tokens you are missing, never their values
nuncio plan        # what would happen. changes nothing.
nuncio sync        # one shot, when the plan looks right
nuncio serve       # or leave it running
```

`nuncio serve` works with **no public address and no webhooks**. It just polls. Add webhooks when
you want sub-second freshness, not because the tool demands them.

## Deploy shapes

| Shape | Ingress | Events | Where |
|---|---|---|---|
| Timer only | none | poll | laptop, NAS, VPS behind NAT — **default** |
| + webhooks | one HTTPS path | push events | VPS with a domain |
| + firehose | outbound only | Tangled events | VPS with no ingress |
| Full | both | everything | maximum freshness |

All four are commented in `deploy/docker-compose.yml`. See [`docs/DEPLOY.md`](docs/DEPLOY.md).

## Multi-repo management

```toml
[policy.site]
platforms = ["github", "gitlab", "codeberg", "srht"]

[policy.paper]
platforms   = ["github", "gitlab"]
authority   = "github"          # intent, not a licence to overwrite

[groups.papers]
policy = "paper"
select = { topic = "paper", platform = "github" }   # resolved by `nuncio discover`

[repos."alice/site".refs."refs/heads/main"]
authority = "github"
```

One daemon, N repositories, per-repo concurrency and locking, resumable across a kill. Groups
select by name list or by topic/visibility so a new repo joins on the next tick instead of needing
a config edit.

## What it models that others flatten

The capability matrix is a first-class object, not an abstraction to work around. `nuncio doctor`
prints it for *your* tokens:

| Asymmetry | Consequence |
|---|---|
| GitHub `refs/pull/*` read-only | never pushed, never an error |
| Forgejo rejects `push --mirror` (deletes default branch) | mirror mode never generated |
| GitLab protected `main` refuses force-push | unprotect → push → re-protect in one journalled group |
| SourceHut `GIT_PRE_RECEIVE` fires before acceptance | push can be **declined before it lands** (see Status) |
| Tangled has no server-side hooks | firehose subscriber instead, `subscribeRepos` |
| Tangled reserves `refs/cobs/*` `refs/atproto/*` … | skipped and reported, not guessed |
| Tangled cannot be created by push-to-unknown-path | `repo.create` called first, create and push reported separately |

## What it deliberately does not do

- **No issue or PR sync.** Platform schemas are incompatible and only one direction is even
  tractable. See `docs/DESIGN.md` §13.
- **No deletions.** Not refs, not repositories, not under any flag. A deletion is a fact to record,
  and propagating one still leaves a snapshot.
- **No automatic conflict resolution.** Conflicts are preserved at a permanent ref and handed to a
  human. Automatic resolution on a bidirectional sync is how history dies.
- **No hub.** There is no "primary" forge unless you name one per ref, and even then the default is
  to park rather than overwrite.

## Safety by construction

- `nuncio init` writes `dry_run = true`. Turning it off is a command you have to type.
- Every push is verified by reading the remote back and comparing SHAs. Exit code zero is not
  evidence.
- Any action that could orphan a commit first writes it to `refs/nuncio/snapshot/<ref>/<ts>`.
  There is no flag that skips this.
- A forge that fails to answer is `unknown`, never `deleted`. An outage can never be mistaken for a
  mass deletion.
- Exit codes: `0` clean, `1` findings for you, `2` operational error, `3` safety refusal.

## Status

Source lives on Tangled. Mirror on GitHub. `nuncio` supports SourceHut as a sync
target, which is where the project started.

> [!IMPORTANT]
> The SourceHut pre-receive gate is **not yet verified** as available to a third-party operator.
> Until it is, sr.ht pushes that would create a divergence are *warned about*, not blocked. Do not
> build a workflow that depends on this being enforcement.

## A note on target policies

`nuncio` mirrors whatever you point it at into whichever forge accepts it. The hosts differ, and in
mid-2026 they started diverging sharply.

- **Codeberg** will not host "projects that mostly consist of code written by generative AI-tools"
  (Terms of Use § 2 (1) 7, voted in by the membership on 23 July 2026). Limited AI assistance is
  fine. A mostly-generated project is not, and there is no "but a human reviewed it" exemption.
- **SourceHut** prohibits "original content written with or which facilitates the use of LLMs or
  other generative AI technologies" (effective for new projects 2026-09-10).
- **git.gay** prohibits it as well.
- **GitHub, GitLab and Tangled** place no restriction on AI-authored projects.

Which of those you may point `nuncio` at is your decision, not the tool's. `nuncio` will not make it
for you silently, and routing around a host's content policy is not something it is built to help
with.

## Docs

- [`docs/DESIGN.md`](docs/DESIGN.md) — full specification, invariants, capability matrix, milestones
- [`docs/ADAPTERS.md`](docs/ADAPTERS.md) — per-forge API notes and verified spike transcripts
- [`docs/DEPLOY.md`](docs/DEPLOY.md) — the five-minute deployment path

## License

MIT.
