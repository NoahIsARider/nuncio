// Package config parses, validates and resolves nuncio.toml.
//
// Secrets never live in this file. Every auth field is a reference such as
// env:NAME, file:/path, ssh:alias or keyring:name. A literal token is a
// validation error, not a warning.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/NoahIsARider/nuncio/internal/caps"
)

// Config is the whole file.
type Config struct {
	Version   int                 `toml:"version"`
	State     State               `toml:"state"`
	Daemon    Daemon              `toml:"daemon"`
	Identity  Identity            `toml:"identity"`
	Platforms map[string]Platform `toml:"platforms"`
	Policy    map[string]Policy   `toml:"policy"`
	Repos     map[string]Repo     `toml:"repos"`
	Groups    map[string]Group    `toml:"groups"`

	// Path is the file the config was loaded from, not a TOML key.
	Path string `toml:"-"`
}

// State holds storage locations.
type State struct {
	Path  string `toml:"path"`
	Cache string `toml:"cache"`
}

// Daemon holds listener and scheduling settings.
type Daemon struct {
	Listen      string `toml:"listen"`
	Interval    string `toml:"interval"`
	Concurrency int    `toml:"concurrency"`
	DryRun      bool   `toml:"dry_run"`
}

// Identity is the author used only for commits nuncio itself creates.
type Identity struct {
	Name  string `toml:"name"`
	Email string `toml:"email"`
}

// Platform is one forge endpoint.
type Platform struct {
	Kind             string `toml:"kind"`
	API              string `toml:"api"`
	Git              string `toml:"git"`
	Auth             string `toml:"auth"`
	Knot             string `toml:"knot"`
	Owner            string `toml:"owner"`
	Firehose         string `toml:"firehose"`
	PreReceiveGate   bool   `toml:"pre_receive_gate"`
	WebhookSecret    string `toml:"webhook_secret"`
	WebhookPublicKey string `toml:"webhook_public_key"`
}

// Policy is a reusable policy block. Pointer fields distinguish "not set" from
// a deliberate zero value.
type Policy struct {
	Platforms     *[]string `toml:"platforms"`
	Authority     *string   `toml:"authority"`
	OnDivergence  *string   `toml:"on_divergence"`
	OnDelete      *string   `toml:"on_delete"`
	Snapshot      *bool     `toml:"snapshot"`
	Verify        *bool     `toml:"verify"`
	DefaultBranch *string   `toml:"default_branch"`
}

// Repo is one managed repository.
type Repo struct {
	Policy        *string            `toml:"policy"`
	Platforms     *[]string          `toml:"platforms"`
	Authority     *string            `toml:"authority"`
	OnDivergence  *string            `toml:"on_divergence"`
	OnDelete      *string            `toml:"on_delete"`
	Snapshot      *bool              `toml:"snapshot"`
	Verify        *bool              `toml:"verify"`
	DefaultBranch *string            `toml:"default_branch"`
	Refs          map[string]RefRule `toml:"refs"`
	// Remote maps a platform name to that platform's "owner/name" coordinates.
	// It exists because the same logical repository legitimately has different
	// names on different forges (Codeberg truncates, SourceHut uses ~user/name).
	Remote map[string]string `toml:"remote"`
}

// RefRule is a per-ref override inside a repo.
type RefRule struct {
	Authority    *string `toml:"authority"`
	OnDivergence *string `toml:"on_divergence"`
	OnDelete     *string `toml:"on_delete"`
}

// Group selects repositories by attributes at reconcile time.
type Group struct {
	Policy string            `toml:"policy"`
	Select map[string]string `toml:"select"`
}

// Divergence modes.
const (
	DivergencePark               = "park"
	DivergenceMerge              = "merge"
	DivergenceForceFromAuthority = "force-from-authority"
)

// Delete modes.
const (
	DeleteIgnore    = "ignore"
	DeletePropagate = "propagate"
)

// AuthorityNone means no platform owns a ref.
const AuthorityNone = "none"

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if unknown := md.Undecoded(); len(unknown) > 0 {
		keys := make([]string, 0, len(unknown))
		for _, k := range unknown {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		c.Path = abs
	} else {
		c.Path = path
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ApplyDefaults fills every field whose absence has a safe, documented default.
func (c *Config) ApplyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.State.Path == "" {
		c.State.Path = filepath.Join(".nuncio", "state.db")
	}
	if c.State.Cache == "" {
		c.State.Cache = filepath.Join(".nuncio", "repos")
	}
	if c.Daemon.Interval == "" {
		c.Daemon.Interval = "15m"
	}
	if c.Daemon.Concurrency == 0 {
		c.Daemon.Concurrency = 4
	}
	if c.Identity.Name == "" {
		c.Identity.Name = "nuncio"
	}
	if c.Identity.Email == "" {
		c.Identity.Email = "nuncio@localhost"
	}
	for name, p := range c.Platforms {
		if p.Knot == "" && p.Kind == string(caps.KindTangled) && p.Git != "" {
			if u, err := url.Parse(p.Git); err == nil {
				p.Knot = u.Hostname()
			}
		}
		if p.Firehose == "" && p.Kind == string(caps.KindTangled) {
			p.Firehose = "wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos"
		}
		c.Platforms[name] = p
	}
}

// Dir returns the directory containing the config file.
func (c *Config) Dir() string {
	if c.Path == "" {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(c.Path)
}

// ResolvePath makes a configured path absolute relative to the config file.
func (c *Config) ResolvePath(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(c.Dir(), p)
}

// StatePath and CachePath return resolved storage locations.
func (c *Config) StatePath() string { return c.ResolvePath(c.State.Path) }
func (c *Config) CachePath() string { return c.ResolvePath(c.State.Cache) }

// Interval parses the reconcile interval.
func (c *Config) Interval() time.Duration {
	d, err := time.ParseDuration(c.Daemon.Interval)
	if err != nil || d <= 0 {
		return 15 * time.Minute
	}
	return d
}

// PlatformNames lists configured platforms deterministically.
func (c *Config) PlatformNames() []string {
	names := make([]string, 0, len(c.Platforms))
	for name := range c.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RepoSlugs lists managed repositories deterministically.
func (c *Config) RepoSlugs() []string {
	slugs := make([]string, 0, len(c.Repos))
	for slug := range c.Repos {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// ValidationError aggregates every problem found in one pass.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid config: " + e.Problems[0]
	}
	return "invalid config:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Validate checks every invariant from the design document.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Version != 1 {
		add("version must be 1, got %d", c.Version)
	}
	if c.State.Path == "" {
		add("state.path must not be empty")
	}
	if c.State.Cache == "" {
		add("state.cache must not be empty")
	}
	if c.State.Path != "" && c.State.Cache != "" {
		if filepath.Clean(c.State.Path) == filepath.Clean(c.State.Cache) {
			add("state.path and state.cache must be different paths")
		}
	}
	if _, err := time.ParseDuration(c.Daemon.Interval); err != nil {
		add("daemon.interval %q is not a duration: %v", c.Daemon.Interval, err)
	} else if d, _ := time.ParseDuration(c.Daemon.Interval); d <= 0 {
		add("daemon.interval must be positive")
	}
	if c.Daemon.Concurrency < 1 {
		add("daemon.concurrency must be at least 1")
	}
	if c.Daemon.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Daemon.Listen); err != nil {
			add("daemon.listen %q is not host:port: %v", c.Daemon.Listen, err)
		}
	}
	if c.Identity.Name == "" || c.Identity.Email == "" {
		add("identity.name and identity.email must not be empty")
	}

	// Platforms.
	for _, name := range c.PlatformNames() {
		p := c.Platforms[name]
		prefix := "platforms." + name
		if !caps.ValidKind(p.Kind) {
			add("%s.kind %q is not one of %s", prefix, p.Kind, strings.Join(caps.Kinds(), ", "))
		}
		if p.API != "" && !validHTTPURL(p.API) {
			add("%s.api %q must be an http(s) URL", prefix, p.API)
		}
		if p.Git != "" && !validGitURL(p.Git) {
			add("%s.git %q must be an ssh://, https://, http://, git:// or file:// URL", prefix, p.Git)
		}
		if p.Auth != "" {
			if err := ValidateAuthRef(p.Auth); err != nil {
				add("%s.auth: %v", prefix, err)
			}
		}
		if p.WebhookSecret != "" {
			if err := ValidateAuthRef(p.WebhookSecret); err != nil {
				add("%s.webhook_secret: %v", prefix, err)
			}
		}
		if p.PreReceiveGate && p.Kind != string(caps.KindSourceHut) {
			add("%s.pre_receive_gate is only meaningful for kind = \"sourcehut\"", prefix)
		}
		if p.Kind == string(caps.KindTangled) && p.Owner == "" {
			add("%s.owner is required for tangled (the repository DID or handle)", prefix)
		}
	}

	// Policy blocks.
	for _, name := range sortedKeys(c.Policy) {
		pol := c.Policy[name]
		prefix := "policy." + name
		c.validatePolicyFields(prefix, pol, add)
		for _, plat := range derefStrings(pol.Platforms) {
			if _, ok := c.Platforms[plat]; !ok {
				add("policy.%s references unknown platform %q", name, plat)
			}
		}
		authority := AuthorityNone
		if pol.Authority != nil {
			authority = *pol.Authority
		}
		if authority != AuthorityNone && !contains(derefStrings(pol.Platforms), authority) {
			add("%s.authority %q is not one of the policy's platforms", prefix, authority)
		}
		if pol.OnDivergence != nil && *pol.OnDivergence == DivergenceForceFromAuthority && authority == AuthorityNone {
			add("%s.on_divergence = force-from-authority requires authority != \"none\"", prefix)
		}
	}

	// Repositories.
	for _, slug := range c.RepoSlugs() {
		r := c.Repos[slug]
		prefix := "repos." + slug
		if !validSlug(slug) {
			add("%s is not an owner/name slug", prefix)
		}
		if r.Policy != nil {
			if _, ok := c.Policy[*r.Policy]; !ok {
				add("%s.policy references unknown policy %q", prefix, *r.Policy)
			}
		}
		platforms := derefStrings(r.Platforms)
		if r.Platforms == nil && r.Policy == nil {
			if _, ok := c.Policy["default"]; !ok {
				add("%s has no platforms and no policy", prefix)
			}
		}
		for _, plat := range platforms {
			if _, ok := c.Platforms[plat]; !ok {
				add("%s references unknown platform %q", prefix, plat)
			}
		}
		// Effective platform set, for authority checks.
		eff, _ := c.PolicyFor(slug)
		effPlatforms := eff.Platforms
		if len(effPlatforms) == 0 {
			effPlatforms = platforms
		}
		if len(effPlatforms) == 0 {
			add("%s resolves to no platforms", prefix)
		}
		if eff.OnDivergence == DivergenceForceFromAuthority && eff.Authority == AuthorityNone {
			add("%s resolves to on_divergence = force-from-authority without an authority", prefix)
		}
		if eff.Authority != AuthorityNone && !contains(effPlatforms, eff.Authority) && len(effPlatforms) > 0 {
			add("%s resolves to authority %q which is not one of its platforms", prefix, eff.Authority)
		}
		if r.Authority != nil && *r.Authority != AuthorityNone {
			if !contains(effPlatforms, *r.Authority) {
				add("%s.authority %q is not one of the repo's platforms", prefix, *r.Authority)
			}
		}
		c.validatePolicyFields(prefix, Policy{
			Authority: r.Authority, OnDivergence: r.OnDivergence, OnDelete: r.OnDelete,
			Snapshot: r.Snapshot, Verify: r.Verify, DefaultBranch: r.DefaultBranch,
		}, add)
		for pattern, rule := range r.Refs {
			rp := prefix + ".refs." + pattern
			if !strings.HasPrefix(pattern, "refs/") {
				add("%s must start with refs/", rp)
			}
			if strings.ContainsAny(pattern, " \t\n") {
				add("%s must not contain whitespace", rp)
			}
			c.validatePolicyFields(rp, Policy{Authority: rule.Authority, OnDivergence: rule.OnDivergence, OnDelete: rule.OnDelete}, add)
			ruleAuthority := eff.Authority
			if rule.Authority != nil {
				ruleAuthority = *rule.Authority
			}
			ruleDivergence := eff.OnDivergence
			if rule.OnDivergence != nil {
				ruleDivergence = *rule.OnDivergence
			}
			if ruleAuthority != AuthorityNone && !contains(effPlatforms, ruleAuthority) {
				add("%s.authority %q is not one of the repo's platforms", rp, ruleAuthority)
			}
			if ruleDivergence == DivergenceForceFromAuthority && ruleAuthority == AuthorityNone {
				add("%s.on_divergence = force-from-authority requires an authority", rp)
			}
		}
		for plat, coords := range r.Remote {
			if _, ok := c.Platforms[plat]; !ok {
				add("%s.remote references unknown platform %q", prefix, plat)
			}
			if !validSlug(coords) {
				add("%s.remote.%s %q is not an owner/name pair", prefix, plat, coords)
			}
		}
	}

	// Groups.
	for _, name := range sortedKeys(c.Groups) {
		g := c.Groups[name]
		if _, ok := c.Policy[g.Policy]; !ok {
			add("groups.%s.policy references unknown policy %q", name, g.Policy)
		}
		for k := range g.Select {
			switch k {
			case "topic", "visibility", "platform", "name":
			default:
				add("groups.%s.select.%s is not a supported selector (topic, visibility, platform, name)", name, k)
			}
		}
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func (c *Config) validatePolicyFields(prefix string, p Policy, add func(string, ...any)) {
	if p.Authority != nil {
		if *p.Authority == "" {
			add("%s.authority must be %q or a platform name", prefix, AuthorityNone)
		}
	}
	if p.OnDivergence != nil {
		switch *p.OnDivergence {
		case DivergencePark, DivergenceMerge, DivergenceForceFromAuthority:
		default:
			add("%s.on_divergence %q must be park, merge or force-from-authority", prefix, *p.OnDivergence)
		}
	}
	if p.OnDelete != nil {
		switch *p.OnDelete {
		case DeleteIgnore, DeletePropagate:
		default:
			add("%s.on_delete %q must be ignore or propagate", prefix, *p.OnDelete)
		}
	}
	if p.Snapshot != nil && !*p.Snapshot {
		add("%s.snapshot = false is not allowed: every overwrite is snapshotted, without a bypass", prefix)
	}
	if p.Verify != nil && !*p.Verify {
		add("%s.verify = false is not allowed: every push is read back and compared", prefix)
	}
	if p.DefaultBranch != nil && *p.DefaultBranch == "" {
		add("%s.default_branch must not be empty", prefix)
	}
}

func validHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func validGitURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "ssh", "https", "http", "git", "file":
		return true
	case "":
		// scp-like syntax: git@host:owner/name.git
		return strings.Contains(s, "@") && strings.Contains(s, ":")
	default:
		return false
	}
}

func validSlug(s string) bool {
	if strings.ContainsAny(s, " \t\n") {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return false
	}
	return parts[0] != "" && parts[1] != ""
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// IsValidationError reports whether err is a config validation failure.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}
