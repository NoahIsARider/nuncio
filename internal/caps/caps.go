// Package caps declares what each forge can and cannot do.
//
// The capability matrix is a first-class object in nuncio: it is declared here,
// validated by `nuncio doctor`, consulted by the reconciler before it emits an
// action, and cited in the report when it blocks one. Nothing is flattened into
// a uniform "remote".
package caps

import (
	"fmt"
	"sort"
	"strings"
)

// Capabilities is the full matrix for one platform.
type Capabilities struct {
	GitPush           bool `json:"git.push"`
	GitPushForce      bool `json:"git.push_force"`
	GitRefDelete      bool `json:"git.ref_delete"`
	GitPushCreateRepo bool `json:"git.push_create_repo"`
	GitProtected      bool `json:"git.protected_branches"`
	GitLFS            bool `json:"git.lfs"`

	RepoCreate bool `json:"repo.create"`
	RepoDelete bool `json:"repo.delete"`

	MetaRead  bool `json:"meta.read"`
	MetaWrite bool `json:"meta.write"`

	EventWebhook  bool `json:"event.webhook"`
	EventFirehose bool `json:"event.firehose"`
	EventGate     bool `json:"event.pre_receive_gate"`
	EventPushOpts bool `json:"event.push_options"`

	IssueRead  bool `json:"issue.read"`
	IssueWrite bool `json:"issue.write"`

	// ReservedRefs are globs the platform owns. Writes to a matching ref are
	// rejected by the platform, so the planner must never generate them.
	ReservedRefs []string `json:"reserved_refs,omitempty"`

	// AuthPush lists the push transports an operator can use.
	AuthPush []string `json:"auth.push,omitempty"`
	// AuthAPI names the API credential model.
	AuthAPI string `json:"auth.api,omitempty"`

	// Limits carries documented per-file/per-repo/per-push/rate limits. A zero
	// value means "not documented", never "unlimited".
	Limits map[string]int64 `json:"limits,omitempty"`
}

// Kind names a supported forge family.
type Kind string

const (
	KindGitHub    Kind = "github"
	KindGitLab    Kind = "gitlab"
	KindForgejo   Kind = "forgejo"
	KindSourceHut Kind = "sourcehut"
	KindTangled   Kind = "tangled"
	KindGit       Kind = "git" // plain git endpoint, used for local/testing setups
)

// Kinds returns every kind accepted by config validation.
func Kinds() []string {
	return []string{string(KindGitHub), string(KindGitLab), string(KindForgejo), string(KindSourceHut), string(KindTangled), string(KindGit)}
}

// ValidKind reports whether kind is a supported forge family.
func ValidKind(kind string) bool {
	for _, k := range Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// Declare returns the declared capability matrix for a kind.
//
// The values are conservative. A capability the platform documents is true; one
// that is unknown is false, because a false capability only makes nuncio skip an
// action, while a wrongly-true one makes it attempt something the forge will
// reject (or worse, accept half of).
func Declare(kind string) (Capabilities, error) {
	switch Kind(kind) {
	case KindGitHub:
		return Capabilities{
			GitPush: true, GitPushForce: true, GitRefDelete: true, GitPushCreateRepo: true,
			GitProtected: true, GitLFS: true,
			RepoCreate: true, RepoDelete: true,
			MetaRead: true, MetaWrite: true,
			EventWebhook: true, EventFirehose: false, EventGate: false, EventPushOpts: false,
			IssueRead: true, IssueWrite: true,
			ReservedRefs: []string{"refs/pull/*"},
			AuthPush:     []string{"ssh_key", "https_pat"},
			AuthAPI:      "pat",
			Limits:       map[string]int64{"rate_per_hour": 5000},
		}, nil
	case KindGitLab:
		return Capabilities{
			GitPush: true, GitPushForce: true, GitRefDelete: true, GitPushCreateRepo: true,
			GitProtected: true, GitLFS: true,
			RepoCreate: true, RepoDelete: true,
			MetaRead: true, MetaWrite: true,
			EventWebhook: true, EventFirehose: false, EventGate: false, EventPushOpts: false,
			IssueRead: true, IssueWrite: true,
			ReservedRefs: []string{"refs/merge-requests/*"},
			AuthPush:     []string{"ssh_key", "https_pat"},
			AuthAPI:      "pat",
			Limits:       map[string]int64{"rate_per_minute": 2000},
		}, nil
	case KindForgejo:
		return Capabilities{
			GitPush: true, GitPushForce: true, GitRefDelete: true, GitPushCreateRepo: true,
			GitProtected: true, GitLFS: true,
			RepoCreate: true, RepoDelete: true,
			MetaRead: true, MetaWrite: true,
			EventWebhook: true, EventFirehose: false, EventGate: false, EventPushOpts: false,
			IssueRead: true, IssueWrite: true,
			ReservedRefs: []string{"refs/pull/*"},
			AuthPush:     []string{"ssh_key", "https_pat"},
			AuthAPI:      "pat",
		}, nil
	case KindSourceHut:
		return Capabilities{
			GitPush: true, GitPushForce: true, GitRefDelete: true, GitPushCreateRepo: false,
			GitProtected: false, GitLFS: false,
			RepoCreate: true, RepoDelete: true,
			MetaRead: true, MetaWrite: true,
			EventWebhook: true, EventFirehose: false, EventGate: true, EventPushOpts: false,
			IssueRead: false, IssueWrite: false,
			ReservedRefs: nil,
			AuthPush:     []string{"ssh_key"},
			AuthAPI:      "pat",
			Limits:       map[string]int64{"rate_per_hour": 60},
		}, nil
	case KindTangled:
		return Capabilities{
			GitPush: true, GitPushForce: true, GitRefDelete: true, GitPushCreateRepo: false,
			GitProtected: false, GitLFS: true,
			RepoCreate: true, RepoDelete: false,
			MetaRead: true, MetaWrite: true,
			EventWebhook: false, EventFirehose: true, EventGate: false, EventPushOpts: true,
			IssueRead: true, IssueWrite: true,
			ReservedRefs: []string{"refs/cobs/*", "refs/cob-checkpoints/*", "refs/hidden/*", "refs/atproto/*"},
			AuthPush:     []string{"ssh_key", "https_service_token"},
			AuthAPI:      "service_auth",
		}, nil
	case KindGit:
		return Capabilities{
			GitPush: true, GitPushForce: true, GitRefDelete: true,
			RepoCreate: false, RepoDelete: false,
			MetaRead: false, MetaWrite: false,
			AuthPush: []string{"ssh_key", "https_pat", "none"},
			AuthAPI:  "none",
		}, nil
	default:
		return Capabilities{}, fmt.Errorf("unknown platform kind %q", kind)
	}
}

// Reserved returns the first reserved glob matching ref and whether one matched.
func (c Capabilities) Reserved(ref string) (string, bool) {
	for _, glob := range c.ReservedRefs {
		if MatchGlob(glob, ref) {
			return glob, true
		}
	}
	return "", false
}

// MatchGlob matches a ref against a glob in which '*' matches any run of
// characters, including '/'. Git's refspec wildcard behaves this way, and so do
// the reserved namespaces we model.
func MatchGlob(pattern, ref string) bool {
	if pattern == "" {
		return false
	}
	if pattern == ref {
		return true
	}
	return matchGlob([]byte(pattern), []byte(ref))
}

func matchGlob(pattern, ref []byte) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Collapse consecutive stars.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(ref); i++ {
				if matchGlob(pattern, ref[i:]) {
					return true
				}
			}
			return false
		default:
			if len(ref) == 0 || ref[0] != pattern[0] {
				return false
			}
			pattern, ref = pattern[1:], ref[1:]
		}
	}
	return len(ref) == 0
}

// Specificity scores a glob for most-specific-first ordering. Exact matches and
// longer literals win; wildcards make a pattern less specific.
func Specificity(pattern string) int {
	literal := 0
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '*' {
			literal++
		}
	}
	return literal*1000 - strings.Count(pattern, "*")
}

// SortPatterns orders patterns most-specific-first, deterministic on ties.
func SortPatterns(patterns []string) []string {
	out := append([]string(nil), patterns...)
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := Specificity(out[i]), Specificity(out[j])
		if si != sj {
			return si > sj
		}
		return out[i] < out[j]
	})
	return out
}
