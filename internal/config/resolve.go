package config

import (
	"sort"

	"github.com/NoahIsARider/nuncio/internal/caps"
)

// RefPolicy is the effective policy for one ref after specificity matching.
type RefPolicy struct {
	Authority    string
	OnDivergence string
	OnDelete     string
}

// ResolvedRefRule pairs a pattern with its policy overrides.
type ResolvedRefRule struct {
	Pattern string
	Policy  RefPolicy
}

// ResolvedPolicy is a repo's effective policy, with every default filled in.
type ResolvedPolicy struct {
	Name          string
	Platforms     []string
	Authority     string
	OnDivergence  string
	OnDelete      string
	Snapshot      bool
	Verify        bool
	DefaultBranch string
	RefRules      []ResolvedRefRule
}

// ForRef returns the policy for one ref. The most specific matching glob wins;
// among equal specificity, the lexicographically first pattern wins, so the
// outcome is deterministic across runs.
func (p ResolvedPolicy) ForRef(ref string) RefPolicy {
	out := RefPolicy{Authority: p.Authority, OnDivergence: p.OnDivergence, OnDelete: p.OnDelete}
	for _, rule := range p.RefRules {
		if caps.MatchGlob(rule.Pattern, ref) {
			if rule.Policy.Authority != "" {
				out.Authority = rule.Policy.Authority
			}
			if rule.Policy.OnDivergence != "" {
				out.OnDivergence = rule.Policy.OnDivergence
			}
			if rule.Policy.OnDelete != "" {
				out.OnDelete = rule.Policy.OnDelete
			}
			break
		}
	}
	return out
}

// MatchedRule names the rule that decided ref, if any.
func (p ResolvedPolicy) MatchedRule(ref string) (string, bool) {
	for _, rule := range p.RefRules {
		if caps.MatchGlob(rule.Pattern, ref) {
			return rule.Pattern, true
		}
	}
	return "", false
}

// PolicyFor resolves a repo slug into an effective policy.
//
// Order: hard defaults -> [policy.default] -> named policy -> repo block ->
// per-ref glob (at ForRef time). A named policy does not silently inherit from
// [policy.default]'s platform list unless it sets none; the defaults themselves
// are always safe.
func (c *Config) PolicyFor(slug string) (ResolvedPolicy, error) {
	out := ResolvedPolicy{
		Name:          "default",
		Authority:     AuthorityNone,
		OnDivergence:  DivergencePark,
		OnDelete:      DeleteIgnore,
		Snapshot:      true,
		Verify:        true,
		DefaultBranch: "main",
	}
	if def, ok := c.Policy["default"]; ok {
		applyPolicy(&out, def)
	}
	repo, ok := c.Repos[slug]
	if !ok {
		return out, nil
	}
	if repo.Policy != nil && *repo.Policy != "default" {
		if named, ok := c.Policy[*repo.Policy]; ok {
			out.Name = *repo.Policy
			applyPolicy(&out, named)
		}
	}
	applyPolicy(&out, Policy{
		Platforms: repo.Platforms, Authority: repo.Authority, OnDivergence: repo.OnDivergence,
		OnDelete: repo.OnDelete, Snapshot: repo.Snapshot, Verify: repo.Verify,
		DefaultBranch: repo.DefaultBranch,
	})
	if len(out.Platforms) == 0 {
		if def, ok := c.Policy["default"]; ok {
			out.Platforms = derefStrings(def.Platforms)
		}
	}
	out.RefRules = nil
	for pattern, rule := range repo.Refs {
		out.RefRules = append(out.RefRules, ResolvedRefRule{
			Pattern: pattern,
			Policy: RefPolicy{
				Authority:    derefString(rule.Authority),
				OnDivergence: derefString(rule.OnDivergence),
				OnDelete:     derefString(rule.OnDelete),
			},
		})
	}
	sort.SliceStable(out.RefRules, func(i, j int) bool {
		si, sj := caps.Specificity(out.RefRules[i].Pattern), caps.Specificity(out.RefRules[j].Pattern)
		if si != sj {
			return si > sj
		}
		return out.RefRules[i].Pattern < out.RefRules[j].Pattern
	})
	return out, nil
}

func applyPolicy(out *ResolvedPolicy, p Policy) {
	if p.Platforms != nil {
		out.Platforms = append([]string(nil), (*p.Platforms)...)
	}
	if p.Authority != nil {
		out.Authority = *p.Authority
	}
	if p.OnDivergence != nil {
		out.OnDivergence = *p.OnDivergence
	}
	if p.OnDelete != nil {
		out.OnDelete = *p.OnDelete
	}
	if p.Snapshot != nil {
		out.Snapshot = *p.Snapshot
	}
	if p.Verify != nil {
		out.Verify = *p.Verify
	}
	if p.DefaultBranch != nil {
		out.DefaultBranch = *p.DefaultBranch
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
