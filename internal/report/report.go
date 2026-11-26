// Package report renders the truth: what each ref is, what was done, and how
// many commits are unreachable from every live ref and snapshot.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/state"
)

// PlatformInfo is the heading line for one platform.
type PlatformInfo struct {
	Name        string `json:"name"`
	Coordinates string `json:"coordinates"`
}

// Lost is either a truthful number or an explicit refusal to guess.
type Lost struct {
	Count    int      `json:"count"`
	Complete bool     `json:"complete"`
	Missing  []string `json:"missing,omitempty"`
}

// Outcome is what happened to one action. It is deliberately independent of
// the executor package so the JSON schema stays stable.
type Outcome struct {
	Action   string `json:"action"`
	Platform string `json:"platform"`
	Ref      string `json:"ref"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// Data is everything a report needs.
type Data struct {
	Repo        string
	Platforms   []PlatformInfo
	GeneratedAt time.Time
	DryRun      bool
	Results     []reconcile.RefResult
	Actions     []reconcile.Action
	Outcomes    []Outcome
	Snapshots   []state.Snapshot
	Lost        Lost
}

type jsonReport struct {
	Version      int                   `json:"version"`
	Repo         string                `json:"repo"`
	GeneratedAt  string                `json:"generated_at"`
	DryRun       bool                  `json:"dry_run"`
	Platforms    []PlatformInfo        `json:"platforms"`
	Refs         []reconcile.RefResult `json:"refs"`
	Actions      []reconcile.Action    `json:"actions"`
	Outcomes     []Outcome             `json:"outcomes"`
	LostCommits  *int                  `json:"lost_commits"`
	LostComplete bool                  `json:"lost_complete"`
	LostMissing  []string              `json:"lost_missing,omitempty"`
	Snapshots    []snapshotJSON        `json:"snapshots"`
	MergeRefs    []string              `json:"merge_attempts"`
	Findings     int                   `json:"findings"`
}

type snapshotJSON struct {
	Platform string `json:"platform"`
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	Tag      string `json:"tag"`
	Created  int64  `json:"created_at"`
}

// HasFindings reports whether the report needs a human.
func (d Data) HasFindings() bool {
	return d.Findings() > 0
}

// Findings counts refs that are not plain equality or that carry a blocking
// notice.
func (d Data) Findings() int {
	plan := reconcile.Result{Results: d.Results}
	return plan.Findings()
}

// RenderJSON writes the machine-readable report. When the lost count is
// incomplete it is null, never 0.
func (d Data) RenderJSON(w io.Writer) error {
	refs := d.Results
	if refs == nil {
		refs = []reconcile.RefResult{}
	}
	actions := d.Actions
	if actions == nil {
		actions = []reconcile.Action{}
	}
	outcomes := d.Outcomes
	if outcomes == nil {
		outcomes = []Outcome{}
	}
	snaps := make([]snapshotJSON, 0, len(d.Snapshots))
	for _, s := range d.Snapshots {
		snaps = append(snaps, snapshotJSON{Platform: s.Platform, Ref: s.Ref, SHA: s.SHA, Tag: s.Tag, Created: s.CreatedAt})
	}
	var lost *int
	if d.Lost.Complete {
		v := d.Lost.Count
		lost = &v
	}
	out := jsonReport{
		Version: 1, Repo: d.Repo, GeneratedAt: d.GeneratedAt.UTC().Format(time.RFC3339),
		DryRun: d.DryRun, Platforms: d.Platforms, Refs: refs, Actions: actions, Outcomes: outcomes,
		LostCommits: lost, LostComplete: d.Lost.Complete, LostMissing: d.Lost.Missing,
		Snapshots: snaps, MergeRefs: mergeRefs(d.Actions), Findings: d.Findings(),
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// RenderHuman writes the report in the shape the design document promises.
func (d Data) RenderHuman(w io.Writer) error {
	var b strings.Builder
	if d.DryRun {
		fmt.Fprintf(&b, "%s (dry run)\n", d.Repo)
	} else {
		fmt.Fprintf(&b, "%s\n", d.Repo)
	}
	plats := append([]PlatformInfo{}, d.Platforms...)
	sort.Slice(plats, func(i, j int) bool { return plats[i].Name < plats[j].Name })
	coords := make([]string, 0, len(plats))
	for _, p := range plats {
		coords = append(coords, p.Name+":"+p.Coordinates)
	}
	fmt.Fprintf(&b, "  %s\n\n", strings.Join(coords, "   "))

	width := len("REF")
	for _, r := range d.Results {
		if len(r.Ref) > width {
			width = len(r.Ref)
		}
	}
	if width > 60 {
		width = 60
	}
	if width < 24 {
		width = 24
	}
	fmt.Fprintf(&b, "  %-*s  %-16s  %s\n", width, "REF", "CLASS", "NOTICE")
	if len(d.Results) == 0 {
		fmt.Fprintf(&b, "  %-*s  %-16s  %s\n", width, "(none)", "equal", "-")
	}
	for _, r := range d.Results {
		ref := r.Ref
		if len(ref) > width {
			ref = "…" + ref[len(ref)-(width-1):]
		}
		fmt.Fprintf(&b, "  %-*s  %-16s  %s\n", width, ref, r.Class, noticeFor(r))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  lost commits        %s\n", lostText(d.Lost))
	fmt.Fprintf(&b, "  snapshots written   %s\n", snapshotText(d.Snapshots))
	fmt.Fprintf(&b, "  merge attempts      %s\n", mergeText(d.Actions))
	fmt.Fprintf(&b, "  to resolve          %s\n", resolveText(d.Results))
	_, err := io.WriteString(w, b.String())
	return err
}

func noticeFor(r reconcile.RefResult) string {
	var parts []string
	switch r.Class {
	case reconcile.ClassEqual:
		return "-"
	case reconcile.ClassFastForward:
		from := map[string]string{}
		for _, p := range r.Peers {
			if p.Present {
				from[p.Platform] = short(p.SHA)
			}
		}
		var lagging []string
		for _, a := range r.Actions {
			if a.Kind == reconcile.ActionPush {
				lagging = append(lagging, a.Platform)
			}
		}
		for _, p := range r.Peers {
			if p.Present {
				parts = append(parts, p.Platform+" "+from[p.Platform])
			}
		}
		if len(lagging) > 0 {
			parts = append(parts, "fb "+strings.Join(lagging, ","))
		}
	case reconcile.ClassDivergence, reconcile.ClassHistoryRewrite, reconcile.ClassTagMoved:
		var shas []string
		for _, p := range r.Peers {
			if p.Present {
				shas = append(shas, p.Platform+" "+short(p.SHA))
			}
		}
		parts = append(parts, strings.Join(shas, ", "))
		if hasKind(r.Actions, reconcile.ActionForcePush) {
			parts = append(parts, "force from authority")
		}
		if hasKind(r.Actions, reconcile.ActionMergeAttempt) {
			parts = append(parts, "merge refs")
		}
		if hasNotice(r, "park") {
			parts = append(parts, "parked")
		}
		if root := firstNotice(r, "info"); strings.Contains(root, "distinct tips") {
			// class already names the fact; keep the notice short.
		}
	case reconcile.ClassCreated:
		parts = append(parts, "created on "+actionPlatforms(r.Actions, reconcile.ActionPush))
	case reconcile.ClassDeleted:
		var names []string
		for _, n := range r.Notices {
			if n.Kind == "deleted" {
				names = append(names, n.Text)
			}
		}
		if len(names) == 0 {
			names = append(names, "deleted")
		}
		parts = append(parts, strings.Join(names, ", "))
		if hasKind(r.Actions, reconcile.ActionDeleteRef) {
			parts = append(parts, "propagated")
		}
	case reconcile.ClassNamespaceRejected:
		parts = append(parts, firstNotice(r, "namespace"))
	case reconcile.ClassCapabilityBlocked:
		parts = append(parts, firstNotice(r, "capability"))
	case reconcile.ClassUnreachable:
		parts = append(parts, firstNotice(r, "unreachable"))
	}
	if len(r.Peers) > 0 {
		var missing []string
		for _, p := range r.Peers {
			if !p.Present && !p.Unknown {
				missing = append(missing, p.Platform)
			}
		}
		if len(missing) > 0 && r.Class == reconcile.ClassCreated {
			// already named by "created on".
		}
	}
	if len(parts) == 0 {
		parts = append(parts, firstNotice(r, ""))
	}
	out := strings.TrimSpace(strings.Join(parts, "; "))
	if out == "" {
		out = "-"
	}
	return out
}

func firstNotice(r reconcile.RefResult, kind string) string {
	for _, n := range r.Notices {
		if kind == "" || n.Kind == kind {
			if n.Platform != "" {
				return n.Platform + ": " + n.Text
			}
			return n.Text
		}
	}
	return ""
}

func hasKind(actions []reconcile.Action, kind reconcile.ActionKind) bool {
	for _, a := range actions {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

func hasNotice(r reconcile.RefResult, kind string) bool {
	for _, n := range r.Notices {
		if n.Kind == kind {
			return true
		}
	}
	return false
}

func actionPlatforms(actions []reconcile.Action, kind reconcile.ActionKind) string {
	seen := map[string]bool{}
	var out []string
	for _, a := range actions {
		if a.Kind == kind && !seen[a.Platform] {
			seen[a.Platform] = true
			out = append(out, a.Platform)
		}
	}
	if len(out) == 0 {
		return "?"
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func lostText(l Lost) string {
	if !l.Complete {
		if len(l.Missing) == 0 {
			return "unknown (objects unavailable; refusing to claim 0)"
		}
		shown := l.Missing
		if len(shown) > 3 {
			shown = shown[:3]
		}
		return fmt.Sprintf("unknown (%d objects unavailable: %s)", len(l.Missing), strings.Join(shown, ", "))
	}
	if l.Count == 0 {
		return "0"
	}
	return fmt.Sprintf("%d", l.Count)
}

func snapshotText(snaps []state.Snapshot) string {
	if len(snaps) == 0 {
		return "-"
	}
	byTag := map[string][]string{}
	for _, s := range snaps {
		byTag[s.Tag] = append(byTag[s.Tag], s.Platform)
	}
	tags := make([]string, 0, len(byTag))
	for tag := range byTag {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	var out []string
	for _, tag := range tags {
		plats := byTag[tag]
		sort.Strings(plats)
		out = append(out, tag+" ("+strings.Join(plats, ", ")+")")
	}
	return strings.Join(out, " ")
}

func mergeText(actions []reconcile.Action) string {
	var refs []string
	for _, a := range actions {
		if a.Kind == reconcile.ActionMergeAttempt && a.MergeRef != "" {
			refs = append(refs, a.MergeRef+" ("+a.Platform+")")
		}
	}
	if len(refs) == 0 {
		return "-"
	}
	sort.Strings(refs)
	return strings.Join(refs, " ")
}

func mergeRefs(actions []reconcile.Action) []string {
	var refs []string
	seen := map[string]bool{}
	for _, a := range actions {
		if a.Kind == reconcile.ActionMergeAttempt && a.MergeRef != "" && !seen[a.MergeRef] {
			seen[a.MergeRef] = true
			refs = append(refs, a.MergeRef)
		}
	}
	sort.Strings(refs)
	return refs
}

func resolveText(results []reconcile.RefResult) string {
	var refs []string
	for _, r := range results {
		if r.Class != reconcile.ClassEqual || r.Blocked != "" {
			refs = append(refs, r.Ref)
		}
	}
	if len(refs) == 0 {
		return "-"
	}
	sort.Strings(refs)
	return strings.Join(refs, ", ")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
