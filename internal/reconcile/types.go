// Package reconcile is the pure core of nuncio: given observed refs, policy,
// platforms and ancestry facts, it classifies every ref and returns the actions
// a human has authorised. It performs no I/O, reads no clock and holds no global
// state, so a table-driven test needs no mocks.
package reconcile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

// Class is a ref classification.
type Class string

const (
	ClassEqual             Class = "equal"
	ClassFastForward       Class = "fast-forward"
	ClassHistoryRewrite    Class = "history-rewrite"
	ClassDivergence        Class = "divergence"
	ClassCreated           Class = "created"
	ClassDeleted           Class = "deleted"
	ClassTagMoved          Class = "tag-moved"
	ClassNamespaceRejected Class = "namespace"
	ClassCapabilityBlocked Class = "capability-blocked"
	ClassUnreachable       Class = "unreachable"
)

// Observation is what a platform reported for one ref.
type Observation struct {
	Present bool
	SHA     string
}

// Platform pairs a name with its capability matrix.
type Platform struct {
	Name string
	Caps caps.Capabilities
}

// Prior is the last known state for one ref on one platform.
type Prior struct {
	Present    bool
	SHA        string
	ObservedAt int64
}

// Relation is a known ancestry fact between two SHAs.
type Relation int

const (
	RelationUnknown Relation = iota
	// RelationAncestor means the first SHA is an ancestor of the second in the
	// order the fact was recorded.
	RelationAncestor
	RelationDescendant
	RelationUnrelated
)

// Relations stores ancestry facts keyed by a sorted pair, so lookups are
// direction-aware but storage is not duplicated.
type Relations map[[2]string]Relation

// NewRelations returns an empty fact set.
func NewRelations() Relations { return Relations{} }

// SetAncestor records that a is an ancestor of b.
func (r Relations) SetAncestor(a, b string) {
	r[orderedKey(a, b)] = direction(a, b, RelationAncestor)
}

// SetUnrelated records that a and b share no ancestor relationship.
func (r Relations) SetUnrelated(a, b string) {
	r[orderedKey(a, b)] = RelationUnrelated
}

// Ancestor reports whether a is an ancestor of b, and whether that is known.
func (r Relations) Ancestor(a, b string) (bool, bool) {
	rel, ok := r[orderedKey(a, b)]
	if !ok || rel == RelationUnknown {
		return false, false
	}
	if rel == RelationUnrelated {
		return false, true
	}
	if a == b {
		return true, true
	}
	lo := min(a, b)
	if rel == RelationAncestor {
		return a == lo, true
	}
	// RelationDescendant: the ordered-first SHA is a descendant of the second.
	return a != lo, true
}

func orderedKey(a, b string) [2]string {
	if a <= b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

func direction(a, b string, rel Relation) Relation {
	if a == b {
		return RelationAncestor
	}
	if a < b {
		return rel
	}
	if rel == RelationAncestor {
		return RelationDescendant
	}
	if rel == RelationDescendant {
		return RelationAncestor
	}
	return rel
}

// Input is everything the planner is allowed to know.
type Input struct {
	Repo       string
	Policy     config.ResolvedPolicy
	Platforms  []Platform
	Refs       map[string]map[string]Observation // ref -> platform -> observation
	Prior      map[string]map[string]Prior       // ref -> platform -> last known
	RepoExists map[string]bool                   // platform -> repository exists (absent = assume yes)
	Unknown    map[string]string                 // platform -> listing error
	Relations  Relations
	Rewrites   map[string]bool // ref -> executor judged it a history rewrite
	Bases      map[string]string
	Now        int64
}

// ActionKind enumerates mutations and reports.
type ActionKind string

const (
	ActionCreateRepo   ActionKind = "create-repo"
	ActionSnapshot     ActionKind = "snapshot"
	ActionPush         ActionKind = "push"
	ActionForcePush    ActionKind = "force-push"
	ActionUnprotect    ActionKind = "unprotect"
	ActionReprotect    ActionKind = "reprotect"
	ActionMergeAttempt ActionKind = "merge-attempt"
	ActionDeleteRef    ActionKind = "delete-ref"
	ActionSetMeta      ActionKind = "set-meta"
	ActionReport       ActionKind = "report"
)

// Action is one planned step. Identity for journalling and replay is
// (Platform, Kind, Ref, FromSHA, ToSHA).
type Action struct {
	Kind        ActionKind `json:"kind"`
	Platform    string     `json:"platform,omitempty"`
	Ref         string     `json:"ref,omitempty"`
	FromSHA     string     `json:"from_sha,omitempty"`
	ToSHA       string     `json:"to_sha,omitempty"`
	LeaseSHA    string     `json:"lease_sha,omitempty"`
	SnapshotRef string     `json:"snapshot_ref,omitempty"`
	MergeRef    string     `json:"merge_ref,omitempty"`
	Ours        string     `json:"ours,omitempty"`
	Theirs      string     `json:"theirs,omitempty"`
	SourceSHA   string     `json:"source_sha,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

// Identity is the idempotence key.
func (a Action) Identity() string {
	return strings.Join([]string{a.Platform, string(a.Kind), a.Ref, a.FromSHA, a.ToSHA, a.SnapshotRef}, "\x00")
}

// Destructive reports whether an action can make a SHA unreachable.
func (a Action) Destructive() bool {
	return a.Kind == ActionForcePush || a.Kind == ActionDeleteRef
}

// Notice explains something the planner did not do and why.
type Notice struct {
	Platform string `json:"platform,omitempty"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

// Peer is a platform's reported state for a ref.
type Peer struct {
	Platform string `json:"platform"`
	Present  bool   `json:"present"`
	SHA      string `json:"sha,omitempty"`
	Unknown  bool   `json:"unknown,omitempty"`
}

// RefResult is one row of the report.
type RefResult struct {
	Ref       string   `json:"ref"`
	Class     Class    `json:"class"`
	Base      string   `json:"base,omitempty"`
	Peers     []Peer   `json:"peers"`
	Actions   []Action `json:"actions,omitempty"`
	Notices   []Notice `json:"notices,omitempty"`
	Blocked   string   `json:"blocked,omitempty"`
	Authority string   `json:"authority,omitempty"`
}

// Plan is the complete deterministic output.
type Result struct {
	Repo    string      `json:"repo"`
	Results []RefResult `json:"refs"`
	Actions []Action    `json:"actions"`
}

// Findings counts refs that deserve a human look: anything not cleanly equal,
// plus blocked or unreachable states.
func (p Result) Findings() int {
	n := 0
	for _, r := range p.Results {
		if r.Class != ClassEqual {
			n++
			continue
		}
		counted := false
		for _, notice := range r.Notices {
			switch notice.Kind {
			case "namespace", "capability", "unreachable", "deleted", "park", "merge":
				n++
				counted = true
			}
			if counted {
				break
			}
		}
	}
	return n
}

// HasFindings reports whether the plan contains anything operational.
func (p Result) HasFindings() bool { return p.Findings() > 0 }

// SnapshotRef builds the permanent ref used to preserve a tip. Branch refs keep
// their short name (refs/heads/feat/http2 -> refs/nuncio/snapshot/feat/http2/ts)
// so the README's output stays truthful.
func SnapshotRef(ref string, ts int64) string {
	short := strings.TrimPrefix(ref, "refs/heads/")
	if short == ref {
		short = strings.TrimPrefix(ref, "refs/")
	}
	short = strings.Trim(short, "/")
	return fmt.Sprintf("refs/nuncio/snapshot/%s/%d", short, ts)
}

// MergeRef builds the namespaced ref holding a merge attempt.
func MergeRef(ref string, ts int64) string {
	short := strings.TrimPrefix(ref, "refs/heads/")
	if short == ref {
		short = strings.TrimPrefix(ref, "refs/")
	}
	short = strings.Trim(short, "/")
	return fmt.Sprintf("refs/nuncio/merge/%s/%d", short, ts)
}

// PlanRefs sorts ref names for deterministic output.
func PlanRefs(in Input) []string {
	set := map[string]bool{}
	for ref := range in.Refs {
		set[ref] = true
	}
	for ref := range in.Prior {
		set[ref] = true
	}
	refs := make([]string, 0, len(set))
	for ref := range set {
		if strings.HasPrefix(ref, "refs/nuncio/") {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}
