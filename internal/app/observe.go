package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/cache"
	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/execute"
	"github.com/NoahIsARider/nuncio/internal/forge"
	"github.com/NoahIsARider/nuncio/internal/gitx"
	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/report"
	"github.com/NoahIsARider/nuncio/internal/state"
)

// Observation is what one reconcile could see. A platform that failed to answer
// is listed in Unknown and its refs are absent; it is never treated as empty.
type Observation struct {
	Refs        map[string]map[string]reconcile.Observation
	AllRefs     map[string]map[string]gitx.RemoteRef
	RepoExists  map[string]bool
	Unknown     map[string]string
	Coordinates map[string]string
}

// Prepared is an observation plus the plan derived from it.
type Prepared struct {
	Spec      RepoSpec
	RepoID    int64
	CacheRepo *cache.Repo
	Remote    *execute.GitRemote
	Obs       Observation
	Prior     map[string]map[string]reconcile.Prior
	Plan      reconcile.Result
	Info      []report.PlatformInfo
}

// prepare observes every platform, records what was seen, and runs the pure
// planner. It never mutates a remote.
func (a *App) prepare(ctx context.Context, spec RepoSpec) (*Prepared, error) {
	cacheRepo, err := a.Cache.Open(ctx, spec.Slug)
	if err != nil {
		return nil, fmt.Errorf("open cache for %s: %w", spec.Slug, err)
	}
	now := a.Now()
	repoRow, err := a.Store.EnsureRepo(ctx, spec.Slug, spec.Policy, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("record repo %s: %w", spec.Slug, err)
	}

	// Remember coordinates so webhooks can resolve this repo later.
	for _, platform := range spec.Platforms {
		owner, name, err := coords(spec, platform)
		if err != nil {
			return nil, err
		}
		if err := a.Store.SetRepoPlatform(ctx, state.RepoPlatform{
			RepoID: repoRow.ID, Platform: platform, Owner: owner, Name: name,
		}); err != nil {
			return nil, fmt.Errorf("record coordinates for %s on %s: %w", spec.Slug, platform, err)
		}
	}

	priorRows, err := a.Store.LoadRefState(ctx, repoRow.ID)
	if err != nil {
		return nil, fmt.Errorf("load prior state for %s: %w", spec.Slug, err)
	}
	prior := convertPrior(priorRows)

	obs, err := a.observe(ctx, spec, cacheRepo)
	if err != nil {
		return nil, err
	}
	if err := a.record(ctx, spec, repoRow.ID, priorRows, obs, now.Unix()); err != nil {
		return nil, fmt.Errorf("record observations for %s: %w", spec.Slug, err)
	}

	bindings := map[string]execute.Binding{}
	for _, platform := range spec.Platforms {
		adapter, ok := a.Adapters[platform]
		if !ok {
			continue
		}
		owner, name, err := coords(spec, platform)
		if err != nil {
			return nil, err
		}
		bindings[platform] = execute.Binding{Platform: platform, Adapter: adapter, Owner: owner, Name: name}
	}
	remote := &execute.GitRemote{
		Cache: cacheRepo, Slug: spec.Slug, Bindings: bindings, Identity: a.Config.Identity,
	}

	relations, bases, rewrites := a.computeRelations(ctx, cacheRepo, obs)

	policy, err := a.policyFor(spec)
	if err != nil {
		return nil, err
	}
	var platforms []reconcile.Platform
	for _, name := range policy.Platforms {
		if _, ok := a.Adapters[name]; !ok {
			continue
		}
		platforms = append(platforms, reconcile.Platform{Name: name, Caps: a.platformCaps(name)})
	}

	result := reconcile.Plan(reconcile.Input{
		Repo: spec.Slug, Policy: policy, Platforms: platforms,
		Refs: obs.Refs, Prior: prior, RepoExists: obs.RepoExists,
		Unknown: obs.Unknown, Relations: relations, Rewrites: rewrites,
		Bases: bases, Now: now.Unix(),
	})

	var info []report.PlatformInfo
	for _, name := range policy.Platforms {
		coordinates := obs.Coordinates[name]
		if coordinates == "" {
			if owner, repo, err := coords(spec, name); err == nil {
				coordinates = owner + "/" + repo
			}
		}
		info = append(info, report.PlatformInfo{Name: name, Coordinates: coordinates})
	}

	return &Prepared{
		Spec: spec, RepoID: repoRow.ID, CacheRepo: cacheRepo, Remote: remote,
		Obs: obs, Prior: prior, Plan: result, Info: info,
	}, nil
}

func (a *App) observe(ctx context.Context, spec RepoSpec, cacheRepo *cache.Repo) (Observation, error) {
	obs := Observation{
		Refs: map[string]map[string]reconcile.Observation{}, AllRefs: map[string]map[string]gitx.RemoteRef{},
		RepoExists: map[string]bool{}, Unknown: map[string]string{}, Coordinates: map[string]string{},
	}
	platforms := append([]string(nil), spec.Platforms...)
	sort.Strings(platforms)
	for _, platform := range platforms {
		adapter, ok := a.Adapters[platform]
		if !ok {
			obs.Unknown[platform] = "platform is referenced by policy but not configured"
			continue
		}
		owner, name, err := coords(spec, platform)
		if err != nil {
			obs.Unknown[platform] = err.Error()
			continue
		}
		obs.Coordinates[platform] = owner + "/" + name
		url := adapter.GitURL(owner, name)
		env := adapter.GitEnv()

		exists := true
		apiErr := error(nil)
		if adapter.Kind() != string(caps.KindGit) {
			_, err := adapter.GetRepo(ctx, owner, name)
			switch {
			case err == nil:
			case forge.IsNotFound(err):
				exists = false
			case errors.Is(err, forge.ErrUnsupported):
				exists = true
			default:
				// An SSH-only operator has no API token, so GetRepo fails even
				// when the repository is perfectly reachable. Fall through to
				// ls-remote instead of declaring the platform unknown.
				apiErr = err
			}
		}
		obs.RepoExists[platform] = exists
		if !exists {
			continue
		}

		all, err := cacheRepo.Git.LsRemote(ctx, url, env)
		if err != nil {
			if apiErr != nil {
				obs.Unknown[platform] = apiErr.Error() + "; ls-remote: " + err.Error()
			} else {
				obs.Unknown[platform] = err.Error()
			}
			continue
		}
		obs.AllRefs[platform] = all

		byRef := map[string]reconcile.Observation{}
		var toFetch []string
		for ref, rr := range all {
			if rr.SHA == "" {
				continue
			}
			// HEAD is a symref, not a ref nuncio manages. Every forge rejects
			// a push whose source is a bare commit into refs/heads/HEAD
			// ("you must fully qualify the ref"), and the default branch is
			// carried by repository metadata instead.
			if !strings.HasPrefix(ref, "refs/") {
				continue
			}
			if !strings.HasPrefix(ref, "refs/nuncio/") {
				byRef[ref] = reconcile.Observation{Present: true, SHA: rr.SHA}
			}
			if _, reserved := a.platformCaps(platform).Reserved(ref); reserved {
				// Reserved namespaces are reported but never managed, so they
				// are neither fetched nor added to the lost-commit universe.
				continue
			}
			if cacheRepo.ObservedRef(ctx, platform, ref) != rr.SHA || !cacheRepo.Git.HasObject(ctx, rr.SHA) {
				toFetch = append(toFetch, ref)
			}
		}
		sort.Strings(toFetch)
		a.fetchBestEffort(ctx, cacheRepo, platform, url, env, toFetch)
		if !objectsPresent(ctx, cacheRepo, byRef, a.platformCaps(platform)) {
			// The ref moved between ls-remote and fetch. Acting on the stale SHA
			// would push an object we do not have, or worse, compare against the
			// wrong tip. Treat the platform as unknown for this round.
			obs.Unknown[platform] = "ref changed during observation; refusing to act on a stale SHA"
			continue
		}
		if len(toFetch) > 0 && adapter.Caps().GitLFS && usesLFS(ctx, cacheRepo, byRef, a.platformCaps(platform)) {
			// The refs are here but their large objects may not be. Fetching LFS
			// now is what makes a later `git lfs push --all` meaningful.
			if err := cacheRepo.Git.FetchLFS(ctx, url, env); err != nil {
				obs.Unknown[platform] = "git lfs fetch: " + err.Error()
				continue
			}
		}
		for ref, o := range byRef {
			if obs.Refs[ref] == nil {
				obs.Refs[ref] = map[string]reconcile.Observation{}
			}
			obs.Refs[ref][platform] = o
		}
	}
	return obs, nil
}

// fetchBestEffort tries one batched fetch and falls back to one ref at a time.
// A ref that vanished between ls-remote and fetch must not abort the whole
// reconcile; the next observation will see the new truth.
func (a *App) fetchBestEffort(ctx context.Context, cacheRepo *cache.Repo, platform, url string, env []string, refs []string) {
	if len(refs) == 0 {
		return
	}
	if err := cacheRepo.FetchRefs(ctx, platform, url, env, refs); err == nil {
		return
	}
	for _, ref := range refs {
		if err := cacheRepo.FetchRefs(ctx, platform, url, env, []string{ref}); err != nil {
			a.Logf("fetch %s %s: %v", platform, ref, err)
		}
	}
}

func (a *App) record(ctx context.Context, spec RepoSpec, repoID int64, priorRows map[string]map[string]state.RefState, obs Observation, now int64) error {
	platforms := map[string]bool{}
	for _, p := range spec.Platforms {
		platforms[p] = true
	}
	for _, byPlatform := range priorRows {
		for p := range byPlatform {
			platforms[p] = true
		}
	}
	ordered := make([]string, 0, len(platforms))
	for p := range platforms {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	for _, platform := range ordered {
		if _, down := obs.Unknown[platform]; down {
			continue
		}
		if exists, known := obs.RepoExists[platform]; known && !exists {
			// A 404 is not proof that the refs were deleted; leave the last
			// observation in place. The report will say the repo is missing.
			continue
		}
		refs := map[string]bool{}
		for ref := range obs.Refs {
			refs[ref] = true
		}
		for ref := range priorRows {
			refs[ref] = true
		}
		for ref := range refs {
			if _, reserved := a.platformCaps(platform).Reserved(ref); reserved {
				// Reserved namespaces are not managed: observing a PR ref must
				// never put its commits into the lost-commit universe, because
				// the platform may prune it at any time.
				continue
			}
			if o, ok := obs.Refs[ref][platform]; ok && o.Present && o.SHA != "" {
				if err := a.Store.RecordObservation(ctx, repoID, ref, platform, o.SHA, true, now, ""); err != nil {
					return err
				}
				continue
			}
			if err := a.Store.MarkAbsent(ctx, repoID, ref, platform, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func convertPrior(in map[string]map[string]state.RefState) map[string]map[string]reconcile.Prior {
	out := make(map[string]map[string]reconcile.Prior, len(in))
	for ref, byPlatform := range in {
		out[ref] = make(map[string]reconcile.Prior, len(byPlatform))
		for platform, rs := range byPlatform {
			out[ref][platform] = reconcile.Prior{Present: rs.Present, SHA: rs.SHA, ObservedAt: rs.ObservedAt}
		}
	}
	return out
}

// computeRelations fills the ancestry facts, merge bases and rewrite judgements
// the planner needs. Only refs whose tips can be commits are considered; an
// annotated tag object has no ancestry.
func (a *App) computeRelations(ctx context.Context, cacheRepo *cache.Repo, obs Observation) (reconcile.Relations, map[string]string, map[string]bool) {
	relations := reconcile.NewRelations()
	bases := map[string]string{}
	rewrites := map[string]bool{}
	for ref, byPlatform := range obs.Refs {
		if strings.HasPrefix(ref, "refs/tags/") || reservedEverywhere(a.platformCaps, ref, byPlatform) {
			continue
		}
		shas := distinctObservations(byPlatform)
		if len(shas) < 2 {
			continue
		}
		for i := 0; i < len(shas); i++ {
			for j := i + 1; j < len(shas); j++ {
				a, b := shas[i], shas[j]
				if a == b {
					continue
				}
				aAncestor, aKnown := cacheRepo.Git.IsAncestor(ctx, a, b)
				if aKnown && aAncestor {
					relations.SetAncestor(a, b)
					continue
				}
				bAncestor, bKnown := cacheRepo.Git.IsAncestor(ctx, b, a)
				if bKnown && bAncestor {
					relations.SetAncestor(b, a)
					continue
				}
				if aKnown && bKnown {
					relations.SetUnrelated(a, b)
				}
			}
		}
		if len(shas) == 2 {
			if base, err := cacheRepo.MergeBase(ctx, shas[0], shas[1]); err == nil {
				bases[ref] = base
				if base == "" {
					if rewrite, err := cacheRepo.LooksLikeRewrite(ctx, shas[0], shas[1]); err == nil && rewrite {
						rewrites[ref] = true
					}
				}
			}
		}
	}
	return relations, bases, rewrites
}

func distinctObservations(byPlatform map[string]reconcile.Observation) []string {
	set := map[string]bool{}
	for _, o := range byPlatform {
		if o.Present && o.SHA != "" {
			set[o.SHA] = true
		}
	}
	out := make([]string, 0, len(set))
	for sha := range set {
		out = append(out, sha)
	}
	sort.Strings(out)
	return out
}

func objectsPresent(ctx context.Context, cacheRepo *cache.Repo, refs map[string]reconcile.Observation, declared caps.Capabilities) bool {
	for ref, o := range refs {
		if _, reserved := declared.Reserved(ref); reserved {
			continue
		}
		if o.SHA == "" {
			continue
		}
		if !cacheRepo.Git.HasObject(ctx, o.SHA) {
			return false
		}
	}
	return true
}

func reservedEverywhere(capsFor func(string) caps.Capabilities, ref string, byPlatform map[string]reconcile.Observation) bool {
	if len(byPlatform) == 0 {
		return false
	}
	for platform := range byPlatform {
		if _, reserved := capsFor(platform).Reserved(ref); !reserved {
			return false
		}
	}
	return true
}

func usesLFS(ctx context.Context, cacheRepo *cache.Repo, refs map[string]reconcile.Observation, declared caps.Capabilities) bool {
	for ref, o := range refs {
		if o.SHA == "" {
			continue
		}
		if _, reserved := declared.Reserved(ref); reserved {
			continue
		}
		uses, err := cacheRepo.Git.UsesLFS(ctx, o.SHA)
		if err == nil && uses {
			return true
		}
	}
	return false
}
