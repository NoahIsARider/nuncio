package reconcile

import (
	"testing"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
	shaD = "dddddddddddddddddddddddddddddddddddddddd"
	shaE = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

func plat(t *testing.T, name, kind string) Platform {
	t.Helper()
	c, err := caps.Declare(kind)
	if err != nil {
		t.Fatal(err)
	}
	return Platform{Name: name, Caps: c}
}

func policy(platforms []string, authority, divergence, deletion string) config.ResolvedPolicy {
	if authority == "" {
		authority = config.AuthorityNone
	}
	if divergence == "" {
		divergence = config.DivergencePark
	}
	if deletion == "" {
		deletion = config.DeleteIgnore
	}
	return config.ResolvedPolicy{
		Platforms: platforms, Authority: authority, OnDivergence: divergence,
		OnDelete: deletion, Snapshot: true, Verify: true, DefaultBranch: "main",
	}
}

func obs(pairs map[string]string) map[string]Observation {
	out := map[string]Observation{}
	for p, sha := range pairs {
		out[p] = Observation{Present: sha != "", SHA: sha}
	}
	return out
}

func makeInput(refs map[string]map[string]string, platforms []Platform, pol config.ResolvedPolicy) Input {
	in := Input{
		Repo: "alice/example", Policy: pol, Platforms: platforms,
		Refs: map[string]map[string]Observation{}, Prior: map[string]map[string]Prior{},
		RepoExists: map[string]bool{}, Unknown: map[string]string{},
		Relations: NewRelations(), Rewrites: map[string]bool{}, Bases: map[string]string{},
		Now: 1759000000,
	}
	for ref, byPlatform := range refs {
		in.Refs[ref] = obs(byPlatform)
	}
	return in
}

func resultFor(t *testing.T, p Result, ref string) RefResult {
	t.Helper()
	for _, r := range p.Results {
		if r.Ref == ref {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", ref, p.Results)
	return RefResult{}
}

func hasAction(r RefResult, kind ActionKind, platform string) bool {
	for _, a := range r.Actions {
		if a.Kind == kind && (platform == "" || a.Platform == platform) {
			return true
		}
	}
	return false
}

func assertSnapshotBeforeDestructive(t *testing.T, r RefResult) {
	t.Helper()
	snapshotted := map[string]bool{}
	for _, a := range r.Actions {
		switch a.Kind {
		case ActionSnapshot:
			snapshotted[a.Platform+"|"+a.Ref] = true
		case ActionForcePush, ActionDeleteRef:
			if !snapshotted[a.Platform+"|"+a.Ref] {
				t.Fatalf("%s on %s/%s without a preceding snapshot: %+v", a.Kind, a.Platform, a.Ref, r.Actions)
			}
		}
	}
}

func TestEqualNoActions(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA, "codeberg": shaA},
	}, ps, policy([]string{"github", "codeberg"}, "", "", ""))
	p := Plan(in)
	r := resultFor(t, p, "refs/heads/main")
	if r.Class != ClassEqual {
		t.Fatalf("class = %s", r.Class)
	}
	if len(r.Actions) != 0 {
		t.Fatalf("actions = %+v", r.Actions)
	}
	if p.Findings() != 0 {
		t.Fatalf("findings = %d", p.Findings())
	}
}

func TestFastForwardTwoForge(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaB, "codeberg": shaA},
	}, ps, policy([]string{"github", "codeberg"}, "", "", ""))
	in.Relations.SetAncestor(shaA, shaB)
	p := Plan(in)
	r := resultFor(t, p, "refs/heads/main")
	if r.Class != ClassFastForward {
		t.Fatalf("class = %s", r.Class)
	}
	if len(r.Actions) != 1 || r.Actions[0].Kind != ActionPush || r.Actions[0].Platform != "codeberg" || r.Actions[0].ToSHA != shaB {
		t.Fatalf("actions = %+v", r.Actions)
	}
	if p.Findings() == 0 {
		t.Error("a fast-forward is still something to report")
	}
}

func TestFastForwardThreeForgeLinear(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo"), plat(t, "tangled", "tangled")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaC, "codeberg": shaA, "tangled": shaB},
	}, ps, policy([]string{"github", "codeberg", "tangled"}, "", "", ""))
	in.Relations.SetAncestor(shaA, shaB)
	in.Relations.SetAncestor(shaA, shaC)
	in.Relations.SetAncestor(shaB, shaC)
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassFastForward {
		t.Fatalf("class = %s", r.Class)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("expected two pushes, got %+v", r.Actions)
	}
	for _, a := range r.Actions {
		if a.ToSHA != shaC {
			t.Errorf("pushed %s instead of tip", a.ToSHA)
		}
	}
}

func TestCreatedTracksRepoCreationSeparately(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "srht", "sourcehut")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA},
		"refs/heads/next": {"github": shaB},
	}, ps, policy([]string{"github", "srht"}, "", "", ""))
	in.RepoExists["srht"] = false
	p := Plan(in)
	main := resultFor(t, p, "refs/heads/main")
	if main.Class != ClassCreated {
		t.Fatalf("class = %s", main.Class)
	}
	creates := 0
	var kinds []ActionKind
	for _, a := range main.Actions {
		if a.Kind == ActionCreateRepo {
			creates++
		}
		kinds = append(kinds, a.Kind)
	}
	if creates != 1 || main.Actions[0].Kind != ActionCreateRepo || main.Actions[1].Kind != ActionPush {
		t.Fatalf("creation must precede push and happen once: %+v", main.Actions)
	}
	next := resultFor(t, p, "refs/heads/next")
	for _, a := range next.Actions {
		if a.Kind == ActionCreateRepo {
			t.Errorf("duplicate CreateRepo for the same platform: %+v", next.Actions)
		}
	}
	_ = kinds
}

func TestNamespaceRejected(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/pull/12/head": {"github": shaA},
	}, ps, policy([]string{"github", "codeberg"}, "", "", ""))
	r := resultFor(t, Plan(in), "refs/pull/12/head")
	if r.Class != ClassNamespaceRejected {
		t.Fatalf("class = %s, notices %+v", r.Class, r.Notices)
	}
	if len(r.Actions) != 0 {
		t.Fatalf("actions = %+v", r.Actions)
	}
	if r.Blocked == "" {
		t.Error("blocked reason not recorded")
	}
	if !hasNotice(r, "namespace") {
		t.Errorf("no namespace notice: %+v", r.Notices)
	}
}

func hasNotice(r RefResult, kind string) bool {
	for _, n := range r.Notices {
		if n.Kind == kind {
			return true
		}
	}
	return false
}

func TestCapabilityBlocked(t *testing.T) {
	readOnly := Platform{Name: "readonly", Caps: caps.Capabilities{}}
	ps := []Platform{plat(t, "github", "github"), readOnly}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA},
	}, ps, policy([]string{"github", "readonly"}, "", "", ""))
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassCapabilityBlocked {
		t.Fatalf("class = %s notices=%+v", r.Class, r.Notices)
	}
	if len(r.Actions) != 0 {
		t.Fatalf("actions = %+v", r.Actions)
	}
}

func TestDivergenceParkedSnapshotsBoth(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/feat/http2": {"github": shaA, "codeberg": shaB},
	}, ps, policy([]string{"github", "codeberg"}, "", "", ""))
	in.Relations.SetUnrelated(shaA, shaB)
	in.Bases["refs/heads/feat/http2"] = shaC
	r := resultFor(t, Plan(in), "refs/heads/feat/http2")
	if r.Class != ClassDivergence {
		t.Fatalf("class = %s", r.Class)
	}
	if r.Base != shaC {
		t.Errorf("base = %s", r.Base)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("expected two snapshots, got %+v", r.Actions)
	}
	for _, a := range r.Actions {
		if a.Kind != ActionSnapshot {
			t.Fatalf("park must not mutate branches: %+v", r.Actions)
		}
	}
	if !hasNotice(r, "park") {
		t.Error("park notice missing")
	}
}

func TestAuthorityWithParkDoesNotForce(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	pol := policy([]string{"github", "codeberg"}, "github", config.DivergencePark, "")
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA, "codeberg": shaB},
	}, ps, pol)
	in.Relations.SetUnrelated(shaA, shaB)
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Authority != "github" {
		t.Errorf("authority = %q", r.Authority)
	}
	for _, a := range r.Actions {
		if a.Kind == ActionForcePush {
			t.Fatalf("park + authority must not force: %+v", r.Actions)
		}
	}
}

func TestForceFromAuthoritySnapshotsFirst(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	pol := policy([]string{"github", "codeberg"}, "github", config.DivergenceForceFromAuthority, "")
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA, "codeberg": shaB},
	}, ps, pol)
	in.Relations.SetUnrelated(shaA, shaB)
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassDivergence {
		t.Fatalf("class = %s", r.Class)
	}
	var snapshotIdx, forceIdx = -1, -1
	for i, a := range r.Actions {
		switch a.Kind {
		case ActionSnapshot:
			if a.Platform == "codeberg" && a.FromSHA == shaB {
				snapshotIdx = i
			}
		case ActionForcePush:
			forceIdx = i
		}
	}
	if snapshotIdx < 0 || forceIdx < 0 || snapshotIdx > forceIdx {
		t.Fatalf("snapshot must precede force: %+v", r.Actions)
	}
	force := r.Actions[forceIdx]
	if force.ToSHA != shaA || force.LeaseSHA != shaB || force.Platform != "codeberg" {
		t.Fatalf("force action = %+v", force)
	}
	assertSnapshotBeforeDestructive(t, r)
}

func TestForceProtectedBranchUnprotectGroup(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "gitlab", "gitlab")}
	pol := policy([]string{"github", "gitlab"}, "github", config.DivergenceForceFromAuthority, "")
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA, "gitlab": shaB},
	}, ps, pol)
	in.Relations.SetUnrelated(shaA, shaB)
	r := resultFor(t, Plan(in), "refs/heads/main")
	kinds := []ActionKind{}
	for _, a := range r.Actions {
		kinds = append(kinds, a.Kind)
	}
	want := []ActionKind{ActionSnapshot, ActionUnprotect, ActionForcePush, ActionReprotect}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	assertSnapshotBeforeDestructive(t, r)
}

func TestForceRefusedByCapability(t *testing.T) {
	noForce := Platform{Name: "noforce", Caps: caps.Capabilities{GitPush: true}}
	ps := []Platform{plat(t, "github", "github"), noForce}
	pol := policy([]string{"github", "noforce"}, "github", config.DivergenceForceFromAuthority, "")
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA, "noforce": shaB},
	}, ps, pol)
	in.Relations.SetUnrelated(shaA, shaB)
	r := resultFor(t, Plan(in), "refs/heads/main")
	if hasAction(r, ActionForcePush, "") {
		t.Fatal("force emitted despite capability false")
	}
	if r.Class != ClassCapabilityBlocked {
		t.Fatalf("class = %s", r.Class)
	}
}

func TestDeletedIgnore(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA},
	}, ps, policy([]string{"github", "codeberg"}, "", "", config.DeleteIgnore))
	in.Prior["refs/heads/main"] = map[string]Prior{
		"github":   {Present: true, SHA: shaA},
		"codeberg": {Present: true, SHA: shaA},
	}
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassDeleted {
		t.Fatalf("class = %s", r.Class)
	}
	if len(r.Actions) != 0 {
		t.Fatalf("ignore must not mutate: %+v", r.Actions)
	}
	if !hasNotice(r, "deleted") {
		t.Error("deletion not recorded")
	}
}

func TestDeletedPropagateSnapshotsThenDeletes(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA},
	}, ps, policy([]string{"github", "codeberg"}, "", "", config.DeletePropagate))
	in.Prior["refs/heads/main"] = map[string]Prior{
		"github":   {Present: true, SHA: shaA},
		"codeberg": {Present: true, SHA: shaA},
	}
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassDeleted {
		t.Fatalf("class = %s", r.Class)
	}
	if !hasAction(r, ActionDeleteRef, "github") {
		t.Fatalf("deletion not propagated: %+v", r.Actions)
	}
	assertSnapshotBeforeDestructive(t, r)
}

func TestDeletionAllGoneStillSnapshots(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{}, ps, policy([]string{"github", "codeberg"}, "", "", config.DeletePropagate))
	in.Prior["refs/heads/main"] = map[string]Prior{
		"github": {Present: true, SHA: shaA}, "codeberg": {Present: true, SHA: shaA},
	}
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassDeleted {
		t.Fatalf("class = %s", r.Class)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("snapshots missing: %+v", r.Actions)
	}
	for _, a := range r.Actions {
		if a.Kind != ActionSnapshot {
			t.Fatalf("unexpected action %+v", a)
		}
	}
}

func TestUnreachableNeverDeleted(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{}, ps, policy([]string{"github", "codeberg"}, "", "", config.DeletePropagate))
	in.Unknown["github"] = "connection refused"
	in.Unknown["codeberg"] = "connection refused"
	in.Prior["refs/heads/main"] = map[string]Prior{
		"github": {Present: true, SHA: shaA}, "codeberg": {Present: true, SHA: shaA},
	}
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassUnreachable {
		t.Fatalf("outage must not look like deletion: class = %s", r.Class)
	}
	if len(r.Actions) != 0 {
		t.Fatalf("actions during outage = %+v", r.Actions)
	}
}

func TestPartialOutageDoesNotCreateOnUnknown(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA},
	}, ps, policy([]string{"github", "codeberg"}, "", "", ""))
	in.Unknown["codeberg"] = "timeout"
	r := resultFor(t, Plan(in), "refs/heads/main")
	if hasAction(r, ActionPush, "codeberg") {
		t.Fatal("push emitted toward an unreachable platform")
	}
	if !hasNotice(r, "unreachable") {
		t.Error("outage not recorded")
	}
}

func TestHistoryRewrite(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA, "codeberg": shaB},
	}, ps, policy([]string{"github", "codeberg"}, "", "", ""))
	in.Relations.SetUnrelated(shaA, shaB)
	in.Rewrites["refs/heads/main"] = true
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassHistoryRewrite {
		t.Fatalf("class = %s", r.Class)
	}
	assertSnapshotBeforeDestructive(t, r)
}

func TestTagMoved(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{
		"refs/tags/v1.2.0": {"github": shaB},
	}, ps, policy([]string{"github", "codeberg"}, "", "", ""))
	in.Prior["refs/tags/v1.2.0"] = map[string]Prior{
		"github": {Present: true, SHA: shaA},
	}
	r := resultFor(t, Plan(in), "refs/tags/v1.2.0")
	if r.Class != ClassTagMoved {
		t.Fatalf("class = %s", r.Class)
	}
}

func TestNWayDivergenceNeverResolved(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo"), plat(t, "tangled", "tangled")}
	for _, mode := range []string{config.DivergencePark, config.DivergenceMerge, config.DivergenceForceFromAuthority} {
		pol := policy([]string{"github", "codeberg", "tangled"}, "github", mode, "")
		in := makeInput(map[string]map[string]string{
			"refs/heads/main": {"github": shaA, "codeberg": shaB, "tangled": shaC},
		}, ps, pol)
		in.Relations.SetUnrelated(shaA, shaB)
		in.Relations.SetUnrelated(shaB, shaC)
		in.Relations.SetUnrelated(shaA, shaC)
		in.Bases["refs/heads/main"] = shaD
		r := resultFor(t, Plan(in), "refs/heads/main")
		for _, a := range r.Actions {
			if a.Kind == ActionForcePush || a.Kind == ActionMergeAttempt || a.Kind == ActionPush {
				t.Fatalf("%s: n-way divergence must be parked, got %+v", mode, r.Actions)
			}
		}
		if len(r.Actions) != 3 {
			t.Fatalf("%s: expected three snapshots, got %+v", mode, r.Actions)
		}
	}
}

func TestMergeModePreservesConflicts(t *testing.T) {
	ps := []Platform{plat(t, "github", "github"), plat(t, "codeberg", "forgejo")}
	pol := policy([]string{"github", "codeberg"}, "", config.DivergenceMerge, "")
	in := makeInput(map[string]map[string]string{
		"refs/heads/main": {"github": shaA, "codeberg": shaB},
	}, ps, pol)
	in.Relations.SetUnrelated(shaA, shaB)
	in.Bases["refs/heads/main"] = shaC
	r := resultFor(t, Plan(in), "refs/heads/main")
	if len(r.Actions) != 4 {
		t.Fatalf("actions = %+v", r.Actions)
	}
	merges := 0
	for _, a := range r.Actions {
		if a.Kind == ActionMergeAttempt {
			merges++
			if a.MergeRef == "" || a.FromSHA != shaC {
				t.Errorf("bad merge action %+v", a)
			}
		}
	}
	if merges != 2 {
		t.Fatalf("merge attempts = %d", merges)
	}
}

func TestRefOnlyOnUnknownIsUnreachable(t *testing.T) {
	ps := []Platform{plat(t, "codeberg", "forgejo")}
	in := makeInput(map[string]map[string]string{}, ps, policy([]string{"codeberg"}, "", "", ""))
	in.Unknown["codeberg"] = "502"
	in.Prior["refs/heads/main"] = map[string]Prior{"codeberg": {Present: true, SHA: shaA}}
	r := resultFor(t, Plan(in), "refs/heads/main")
	if r.Class != ClassUnreachable {
		t.Fatalf("class = %s", r.Class)
	}
}

func TestSnapshotRefShape(t *testing.T) {
	if got := SnapshotRef("refs/heads/feat/http2", 1759000000); got != "refs/nuncio/snapshot/feat/http2/1759000000" {
		t.Errorf("snapshot ref = %s", got)
	}
	if got := SnapshotRef("refs/tags/v1.2.0", 12); got != "refs/nuncio/snapshot/tags/v1.2.0/12" {
		t.Errorf("tag snapshot ref = %s", got)
	}
	if got := MergeRef("refs/heads/feat/http2", 7); got != "refs/nuncio/merge/feat/http2/7" {
		t.Errorf("merge ref = %s", got)
	}
}

func TestPlanDeterministicOrder(t *testing.T) {
	ps := []Platform{plat(t, "github", "github")}
	in := makeInput(map[string]map[string]string{
		"refs/heads/z": {"github": shaA},
		"refs/heads/a": {"github": shaA},
	}, ps, policy([]string{"github"}, "", "", ""))
	p := Plan(in)
	if len(p.Results) != 2 || p.Results[0].Ref != "refs/heads/a" {
		t.Fatalf("refs not sorted: %+v", p.Results)
	}
}
