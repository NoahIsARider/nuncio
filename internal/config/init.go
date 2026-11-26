package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Remote is one line of `git remote -v`, normalised into forge coordinates.
type Remote struct {
	Name     string
	URL      string
	Host     string
	Owner    string
	Repo     string
	Platform string
}

// Slug is owner/repo as used for a managed repository key.
func (r Remote) Slug() string { return r.Owner + "/" + r.Repo }

var scpLike = regexp.MustCompile(`^(?:([^@/]+)@)?([^:/]+):(.*)$`)

// ParseRemotes parses `git remote -v` output, keeping fetch entries.
func ParseRemotes(output string) []Remote {
	seen := map[string]bool{}
	var out []Remote
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, raw := fields[0], fields[1]
		if len(fields) >= 3 && fields[2] == "(push)" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		r := parseRemoteURL(raw)
		r.Name = name
		r.URL = raw
		out = append(out, r)
	}
	return out
}

func parseRemoteURL(raw string) Remote {
	var host, path string
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		host, path = u.Hostname(), u.Path
	} else if m := scpLike.FindStringSubmatch(raw); m != nil {
		host, path = m[2], m[3]
	} else {
		return Remote{}
	}
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var owner, repo string
	switch {
	case len(parts) >= 2:
		owner, repo = parts[len(parts)-2], parts[len(parts)-1]
	case len(parts) == 1:
		repo = parts[0]
	}
	// SourceHut uses ~user/repo; the tilde belongs to the owner.
	owner = strings.TrimPrefix(owner, "~")
	return Remote{
		Host:     host,
		Owner:    owner,
		Repo:     repo,
		Platform: DetectPlatform(host),
	}
}

// DetectPlatform maps a host to the config key used by init. Unknown hosts
// return "".
func DetectPlatform(host string) string {
	host = strings.ToLower(host)
	switch {
	case host == "github.com":
		return "github"
	case host == "gitlab.com" || strings.Contains(host, "gitlab"):
		return "gitlab"
	case host == "codeberg.org" || strings.Contains(host, "forgejo"):
		return "codeberg"
	case host == "git.sr.ht" || strings.HasSuffix(host, ".sr.ht"):
		return "srht"
	case host == "tangled.org" || strings.HasSuffix(host, ".tangled.org") || strings.HasSuffix(host, ".tngl.sh"):
		return "tangled"
	default:
		return ""
	}
}

// PlatformPreset returns the canonical block for a forge key.
func PlatformPreset(platform string, known Remote) (string, Platform, bool) {
	switch platform {
	case "github":
		return "github", Platform{Kind: "github", API: "https://api.github.com", Git: "https://github.com", Auth: "env:GITHUB_TOKEN"}, true
	case "gitlab":
		return "gitlab", Platform{Kind: "gitlab", API: "https://gitlab.com/api/v4", Git: "ssh://git@gitlab.com", Auth: "env:GITLAB_TOKEN"}, true
	case "codeberg", "forgejo":
		return "codeberg", Platform{Kind: "forgejo", API: "https://codeberg.org/api/v1", Git: "ssh://git@codeberg.org", Auth: "ssh:codeberg"}, true
	case "srht", "sourcehut":
		return "srht", Platform{Kind: "sourcehut", API: "https://git.sr.ht/query", Git: "ssh://git@git.sr.ht", Auth: "env:SRHT_TOKEN"}, true
	case "tangled":
		host := "tangled.org"
		if known.Host != "" {
			host = known.Host
		}
		owner := known.Owner
		return "tangled", Platform{
			Kind: "tangled", Knot: host, Git: "ssh://git@" + host, Owner: owner,
			Auth: "ssh:tangled", Firehose: "wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos",
		}, true
	default:
		return "", Platform{}, false
	}
}

// InitOptions controls config generation.
type InitOptions struct {
	Remotes  []Remote
	Forge    []string // additional forge keys requested by --forge
	Interval string
}

// GenerateInit builds a safe-by-construction config from detected remotes.
func GenerateInit(opts InitOptions) (*Config, string, error) {
	if len(opts.Remotes) == 0 {
		return nil, "", fmt.Errorf("no git remotes detected; run nuncio init inside a clone or pass --forge with a platform to scaffold")
	}
	c := &Config{Version: 1}
	c.State = State{Path: ".nuncio/state.db", Cache: ".nuncio/repos"}
	interval := opts.Interval
	if interval == "" {
		interval = "15m"
	}
	c.Daemon = Daemon{Listen: "127.0.0.1:8787", Interval: interval, Concurrency: 4, DryRun: true}
	c.Identity = Identity{Name: "nuncio", Email: "nuncio@localhost"}
	c.Platforms = map[string]Platform{}

	// Primary repository: the first remote whose host we recognise.
	var primary *Remote
	for i := range opts.Remotes {
		if opts.Remotes[i].Platform != "" && opts.Remotes[i].Owner != "" && opts.Remotes[i].Repo != "" {
			primary = &opts.Remotes[i]
			break
		}
	}
	if primary == nil {
		return nil, "", fmt.Errorf("none of the git remotes point at a recognised forge (github, gitlab, codeberg, sr.ht, tangled)")
	}

	// Platform blocks for every remote we recognised.
	for _, r := range opts.Remotes {
		if r.Platform == "" || r.Owner == "" || r.Repo == "" {
			continue
		}
		if _, ok := c.Platforms[r.Platform]; ok {
			continue
		}
		key, block, ok := PlatformPreset(r.Platform, r)
		if !ok {
			continue
		}
		c.Platforms[key] = block
	}
	// Additional forges named on the command line.
	for _, f := range opts.Forge {
		key, block, ok := PlatformPreset(strings.ToLower(f), Remote{Host: "tangled.org"})
		if !ok {
			return nil, "", fmt.Errorf("--forge %q is not a supported forge key (github, gitlab, codeberg, srht, tangled)", f)
		}
		if _, exists := c.Platforms[key]; exists {
			continue
		}
		if block.Kind == "tangled" && block.Owner == "" {
			block.Owner = primary.Owner
		}
		c.Platforms[key] = block
	}

	platformNames := make([]string, 0, len(c.Platforms))
	for name := range c.Platforms {
		platformNames = append(platformNames, name)
	}
	sort.Strings(platformNames)
	c.Policy = map[string]Policy{
		"default": {
			Platforms:    &platformNames,
			Authority:    strPtr(AuthorityNone),
			OnDivergence: strPtr(DivergencePark),
			OnDelete:     strPtr(DeleteIgnore),
			Snapshot:     boolPtr(true),
			Verify:       boolPtr(true),
		},
	}

	slug := primary.Slug()
	c.Repos = map[string]Repo{
		slug: {
			Policy: strPtr("default"),
			Remote: map[string]string{},
		},
	}
	for _, r := range opts.Remotes {
		if r.Platform == "" {
			continue
		}
		repo := c.Repos[slug]
		if repo.Remote == nil {
			repo.Remote = map[string]string{}
		}
		repo.Remote[r.Platform] = r.Slug()
		c.Repos[slug] = repo
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, "", err
	}
	return c, slug, nil
}

// AddRepoBlock returns the TOML text for one discovered repository.
func AddRepoBlock(slug, policy string, platforms []string) string {
	if policy == "" {
		policy = "default"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n[repos.%s]\n", quoteKey(slug))
	fmt.Fprintf(&b, "policy = %s\n", quote(policy))
	if len(platforms) > 0 {
		fmt.Fprintf(&b, "platforms = %s\n", stringList(platforms))
	}
	return b.String()
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
