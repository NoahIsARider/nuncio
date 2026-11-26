// Package app wires nuncio's pure reconciler to the world: configuration,
// adapters, the object cache, SQLite state, the journalled executor and the
// report. It is the layer the CLI and the daemon call into; it never decides
// sync semantics itself.
package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/NoahIsARider/nuncio/internal/cache"
	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/forge"
	"github.com/NoahIsARider/nuncio/internal/state"
)

// App is one operator's nuncio: one config, one store, one cache, one adapter
// per configured platform.
type App struct {
	Config   *config.Config
	Store    *state.Store
	Cache    *cache.Cache
	Adapters map[string]forge.Adapter

	Logf     func(format string, args ...any)
	Now      func() time.Time
	warnings []string
}

// Options controls construction.
type Options struct {
	Logf func(format string, args ...any)
	Now  func() time.Time
}

// Open builds an App. Missing secrets do not prevent startup: a platform whose
// token is absent is reported by doctor and treated as unreachable by the
// planner, which is safer than refusing to run for a repo that only uses SSH.
func Open(ctx context.Context, cfg *config.Config, opts Options) (*App, error) {
	if cfg == nil {
		return nil, errors.New("app: nil config")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	store, err := state.Open(cfg.StatePath())
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	a := &App{
		Config: cfg, Store: store, Cache: cache.New(cfg.CachePath()),
		Adapters: map[string]forge.Adapter{}, Logf: logf, Now: now,
	}
	for _, name := range cfg.PlatformNames() {
		p := cfg.Platforms[name]
		ref, err := config.ParseAuthRef(p.Auth)
		if err != nil {
			a.Close()
			return nil, fmt.Errorf("platforms.%s.auth: %w", name, err)
		}
		secret := ""
		switch ref.Scheme {
		case "":
		case "env", "file":
			v, err := config.Resolve(p.Auth)
			if err != nil {
				a.warnings = append(a.warnings, fmt.Sprintf("platform %s: %s is not available (%v)", name, config.SecretName(p.Auth), err))
			} else {
				secret = v
			}
		case "ssh":
			// SSH is consumed by git through GIT_SSH_COMMAND; there is no bearer.
		case "keyring":
			a.warnings = append(a.warnings, fmt.Sprintf("platform %s: keyring credentials are not available on this host", name))
		}
		adapter, err := forge.New(ctx, name, p, forge.Auth{Ref: ref, Secret: secret})
		if err != nil {
			a.Close()
			return nil, fmt.Errorf("platforms.%s: %w", name, err)
		}
		a.Adapters[name] = adapter
		endpoint := p.API
		if endpoint == "" {
			endpoint = p.Git
		}
		if err := store.UpsertPlatform(ctx, name, p.Kind, endpoint); err != nil {
			a.Close()
			return nil, fmt.Errorf("record platform %s: %w", name, err)
		}
	}
	return a, nil
}

// Close releases adapters and the state store.
func (a *App) Close() error {
	var first error
	for _, name := range sortedAdapterNames(a.Adapters) {
		if err := a.Adapters[name].Close(); err != nil && first == nil {
			first = err
		}
	}
	if a.Store != nil {
		if err := a.Store.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Warnings returns construction-time warnings, which name secrets but never
// values.
func (a *App) Warnings() []string {
	return append([]string(nil), a.warnings...)
}

// RepoSpec is one managed repository with resolved coordinates. Remotes maps a
// platform name to that platform's owner/name, because the same logical repo
// legitimately has different names on different forges.
type RepoSpec struct {
	Slug      string
	Policy    string
	Platforms []string
	Remotes   map[string]string
	Generated bool
	Group     string
}

// RepoSpecs resolves static repositories and group selectors. It never removes
// a repository that was managed before: a selector that stops matching is
// reported, and the repo remains managed, as the design requires.
func (a *App) RepoSpecs(ctx context.Context) ([]RepoSpec, []string, error) {
	specs := map[string]RepoSpec{}
	var notices []string

	// Static repositories.
	for _, slug := range a.Config.RepoSlugs() {
		repo := a.Config.Repos[slug]
		resolved, err := a.Config.PolicyFor(slug)
		if err != nil {
			return nil, nil, err
		}
		platforms := append([]string(nil), resolved.Platforms...)
		if len(platforms) == 0 {
			platforms = a.Config.PlatformNames()
		}
		remotes := map[string]string{}
		for platform, coords := range repo.Remote {
			remotes[platform] = coords
		}
		for _, platform := range platforms {
			if remotes[platform] == "" {
				remotes[platform] = slug
			}
		}
		specs[slug] = RepoSpec{
			Slug: slug, Policy: resolved.Name, Platforms: platforms, Remotes: remotes,
		}
	}

	// Group selectors. A failure to list one platform is a notice, not a reason
	// to lose the repositories that are already managed.
	for _, group := range sortedGroupNames(a.Config.Groups) {
		g := a.Config.Groups[group]
		pol, ok := a.Config.Policy[g.Policy]
		if !ok {
			notices = append(notices, fmt.Sprintf("group %s references unknown policy %q", group, g.Policy))
			continue
		}
		platforms := derefStrings(pol.Platforms)
		if len(platforms) == 0 {
			if def, ok := a.Config.Policy["default"]; ok {
				platforms = derefStrings(def.Platforms)
			}
		}
		if len(platforms) == 0 {
			platforms = a.Config.PlatformNames()
		}
		queryPlatforms := platforms
		if only := g.Select["platform"]; only != "" {
			queryPlatforms = []string{only}
		}
		for _, platform := range queryPlatforms {
			adapter, ok := a.Adapters[platform]
			if !ok {
				notices = append(notices, fmt.Sprintf("group %s: platform %q is not configured", group, platform))
				continue
			}
			listed, err := adapter.ListRepos(ctx)
			if err != nil {
				notices = append(notices, fmt.Sprintf("group %s: list %s: %v", group, platform, err))
				continue
			}
			for _, repo := range listed {
				if !matchSelectors(repo, g.Select) {
					continue
				}
				slug := repo.Owner + "/" + repo.Name
				if spec, exists := specs[slug]; exists {
					// A static block always wins; a generated block keeps the
					// coordinates learned from each platform.
					if spec.Generated {
						spec.Remotes[platform] = slug
						specs[slug] = spec
					}
					continue
				}
				spec := RepoSpec{
					Slug: slug, Policy: g.Policy, Platforms: append([]string(nil), platforms...),
					Remotes: map[string]string{}, Generated: true, Group: group,
				}
				for _, p := range platforms {
					spec.Remotes[p] = slug
				}
				spec.Remotes[platform] = slug
				specs[slug] = spec
			}
		}
	}

	// Repositories remembered in state but no longer selected stay managed.
	if stored, err := a.Store.ListRepos(ctx); err == nil {
		for _, row := range stored {
			if _, ok := specs[row.Slug]; ok {
				continue
			}
			coords, err := a.Store.RepoPlatforms(ctx, row.ID)
			if err != nil || len(coords) == 0 {
				continue
			}
			spec := RepoSpec{Slug: row.Slug, Policy: row.Policy, Remotes: map[string]string{}}
			for _, c := range coords {
				spec.Platforms = append(spec.Platforms, c.Platform)
				spec.Remotes[c.Platform] = c.Owner + "/" + c.Name
			}
			sort.Strings(spec.Platforms)
			specs[row.Slug] = spec
			notices = append(notices, fmt.Sprintf("repo %s is remembered from a previous run and is still managed; its selector no longer matches", row.Slug))
		}
	}

	out := make([]RepoSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	sort.Strings(notices)
	return out, notices, nil
}

// Spec resolves one repository by slug.
func (a *App) Spec(ctx context.Context, slug string) (RepoSpec, error) {
	specs, _, err := a.RepoSpecs(ctx)
	if err != nil {
		return RepoSpec{}, err
	}
	for _, spec := range specs {
		if spec.Slug == slug {
			return spec, nil
		}
	}
	return RepoSpec{}, fmt.Errorf("repo %q is not managed by this config", slug)
}

// policyFor resolves a repository's effective policy, including repos that only
// exist because a group selected them.
func (a *App) policyFor(spec RepoSpec) (config.ResolvedPolicy, error) {
	if _, ok := a.Config.Repos[spec.Slug]; ok {
		return a.Config.PolicyFor(spec.Slug)
	}
	clone := *a.Config
	clone.Repos = make(map[string]config.Repo, len(a.Config.Repos)+1)
	for k, v := range a.Config.Repos {
		clone.Repos[k] = v
	}
	policy := spec.Policy
	if _, ok := a.Config.Policy[policy]; !ok {
		// A remembered selector whose policy was later removed must not take
		// the whole daemon down; fall back to the default policy.
		policy = "default"
	}
	platforms := append([]string(nil), spec.Platforms...)
	clone.Repos[spec.Slug] = config.Repo{Policy: &policy, Platforms: &platforms}
	return clone.PolicyFor(spec.Slug)
}

// coords splits a RepoSpec's coordinates for one platform.
func coords(spec RepoSpec, platform string) (owner, name string, err error) {
	raw := spec.Remotes[platform]
	if raw == "" {
		raw = spec.Slug
	}
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repo %s has invalid coordinates %q for platform %s", spec.Slug, raw, platform)
	}
	return parts[0], parts[1], nil
}

// platformCaps returns the declared capability matrix for a platform, or an
// empty matrix when the platform is not configured.
func (a *App) platformCaps(platform string) caps.Capabilities {
	if adapter, ok := a.Adapters[platform]; ok {
		return adapter.Caps()
	}
	return caps.Capabilities{}
}

func matchSelectors(repo forge.Repo, sel map[string]string) bool {
	for key, want := range sel {
		switch key {
		case "platform":
			// The query platform is already constrained by the caller.
		case "topic":
			if !containsFold(repo.Topics, want) {
				return false
			}
		case "visibility":
			vis := repo.Visibility
			if vis == "" {
				if repo.Private {
					vis = "private"
				} else {
					vis = "public"
				}
			}
			if !strings.EqualFold(vis, want) {
				return false
			}
		case "name":
			if !caps.MatchGlob(want, repo.Name) && !caps.MatchGlob(want, repo.Owner+"/"+repo.Name) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func derefStrings(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

func sortedGroupNames(groups map[string]config.Group) []string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedAdapterNames(m map[string]forge.Adapter) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
