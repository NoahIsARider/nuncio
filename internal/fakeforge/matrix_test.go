package fakeforge

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/execute"
	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/state"
)

const (
	s1 = "1111111111111111111111111111111111111111"
	s2 = "2222222222222222222222222222222222222222"
	s3 = "3333333333333333333333333333333333333333"
	s4 = "4444444444444444444444444444444444444444"
	s5 = "5555555555555555555555555555555555555555"
)

func seededForges(p Profile) (*Forge, *Forge) {
	pa := p
	pa.Name = "a"
	pb := p
	pb.Name = "b"
	a, b := New(pa), New(pb)
	for _, f := range []*Forge{a, b} {
		f.AddCommit(s1)
		f.AddCommit(s2, s1)
		f.AddCommit(s3, s2)
		f.AddCommit(s4, s2)
		f.AddCommit(s5, s3)
	}
	return a, b
}

type scenario struct {
	name       string
	seed       func(a, b *Forge)
	prior      map[string]map[string]reconcile.Prior
	authority  string
	divergence string
	deletion   string
	relations  func(r reconcile.Relations)
	bases      map[string]string
}

func refPrior(sha string) map[string]map[string]reconcile.Prior {
	return map[string]map[string]reconcile.Prior{
		"refs/heads/main": {
			"a": {Present: true, SHA: sha, ObservedAt: 1},
			"b": {Present: true, SHA: sha, ObservedAt: 1},
		},
	}
}

func scenarios() []scenario {
	return []scenario{
		{name: "equal", seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s2)
			b.SeedRef("refs/heads/main", s2)
		}},
		{name: "fast-forward", seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s2)
			b.SeedRef("refs/heads/main", s1)
		}, relations: func(r reconcile.Relations) { r.SetAncestor(s1, s2) }},
		{name: "divergence", seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s3)
			b.SeedRef("refs/heads/main", s4)
		}, relations: func(r reconcile.Relations) { r.SetUnrelated(s3, s4) }, bases: map[string]string{"refs/heads/main": s2}},
		{name: "authority", seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s3)
			b.SeedRef("refs/heads/main", s4)
		}, authority: "a", divergence: config.DivergenceForceFromAuthority,
			relations: func(r reconcile.Relations) { r.SetUnrelated(s3, s4) }, bases: map[string]string{"refs/heads/main": s2}},
		{name: "created", seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s2)
		}},
		{name: "missing-repo", seed: func(a, b *Forge) {
			b.Profile.MissingRepo = true
			b.repoExist = false
			a.SeedRef("refs/heads/main", s2)
		}},
		{name: "deletion", seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s2)
		}, prior: refPrior(s2), deletion: config.DeletePropagate},
		{name: "namespace", seed: func(a, b *Forge) {
			b.Profile.Reserved = append(append([]string{}, b.Profile.Reserved...), "refs/pull/*")
			b.Caps.ReservedRefs = b.Profile.Reserved
			a.SeedRef("refs/pull/1/head", s5)
		}},
		{name: "list-error", seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s2)
			b.SeedRef("refs/heads/main", s3)
			b.Profile.ListError = true
		}, prior: map[string]map[string]reconcile.Prior{
			"refs/heads/main": {
				"a": {Present: true, SHA: s2, ObservedAt: 1},
				"b": {Present: true, SHA: s3, ObservedAt: 1},
			},
		}, deletion: config.DeletePropagate},
	}
}

func policyFor(sc scenario) config.ResolvedPolicy {
	authority := sc.authority
	if authority == "" {
		authority = config.AuthorityNone
	}
	div := sc.divergence
	if div == "" {
		div = config.DivergencePark
	}
	del := sc.deletion
	if del == "" {
		del = config.DeleteIgnore
	}
	return config.ResolvedPolicy{
		Platforms: []string{"a", "b"}, Authority: authority,
		OnDivergence: div, OnDelete: del, Snapshot: true, Verify: true, DefaultBranch: "main",
	}
}

func observe(a, b *Forge) (map[string]map[string]reconcile.Observation, map[string]string, map[string]bool) {
	refs := map[string]map[string]reconcile.Observation{}
	unknown := map[string]string{}
	exists := map[string]bool{}
	for _, f := range []*Forge{a, b} {
		ok, _ := f.RepoExists(context.Background(), "", "")
		exists[f.Profile.Name] = ok
		list, err := f.List()
		if err != nil {
			unknown[f.Profile.Name] = err.Error()
			continue
		}
		for ref, sha := range list {
			if strings.HasPrefix(ref, "refs/nuncio/") {
				continue
			}
			if refs[ref] == nil {
				refs[ref] = map[string]reconcile.Observation{}
			}
			refs[ref][f.Profile.Name] = reconcile.Observation{Present: true, SHA: sha}
		}
	}
	return refs, unknown, exists
}

func buildInput(sc scenario, a, b *Forge, now int64) reconcile.Input {
	refs, unknown, exists := observe(a, b)
	rel := reconcile.NewRelations()
	if sc.relations != nil {
		sc.relations(rel)
	}
	return reconcile.Input{
		Repo: "alice/matrix", Policy: policyFor(sc),
		Platforms: []reconcile.Platform{
			{Name: "a", Caps: a.Caps},
			{Name: "b", Caps: b.Caps},
		},
		Refs: refs, Prior: sc.prior, RepoExists: exists, Unknown: unknown,
		Relations: rel, Rewrites: map[string]bool{}, Bases: sc.bases, Now: now,
	}
}

func assertCapabilities(t *testing.T, plan reconcile.Result, forges map[string]*Forge) {
	t.Helper()
	for _, action := range plan.Actions {
		f, ok := forges[action.Platform]
		if !ok {
			t.Fatalf("action targets unknown platform %q: %+v", action.Platform, action)
		}
		c := f.Caps
		need := func(ok bool, what string) {
			if !ok {
				t.Fatalf("action %s on %s requires %s, which is false: %+v", action.Kind, action.Platform, what, action)
			}
		}
		switch action.Kind {
		case reconcile.ActionPush, reconcile.ActionSnapshot, reconcile.ActionMergeAttempt:
			need(c.GitPush, "git.push")
		case reconcile.ActionForcePush:
			need(c.GitPush, "git.push")
			need(c.GitPushForce, "git.push_force")
		case reconcile.ActionDeleteRef:
			need(c.GitPush, "git.push")
			need(c.GitRefDelete, "git.ref_delete")
		case reconcile.ActionCreateRepo:
			need(c.RepoCreate, "repo.create")
		case reconcile.ActionUnprotect, reconcile.ActionReprotect:
			need(c.GitProtected, "git.protected_branches")
		}
		if action.Ref != "" {
			if glob, reserved := c.Reserved(action.Ref); reserved {
				t.Fatalf("action touches reserved namespace %s on %s: %+v", glob, action.Platform, action)
			}
		}
		if action.SnapshotRef != "" {
			if glob, reserved := c.Reserved(action.SnapshotRef); reserved {
				t.Fatalf("snapshot touches reserved namespace %s: %+v", glob, action)
			}
		}
	}
}

func assertNoActionsToUnknown(t *testing.T, plan reconcile.Result, unknown map[string]string) {
	t.Helper()
	for _, action := range plan.Actions {
		if errText, down := unknown[action.Platform]; down {
			t.Fatalf("action sent to unreachable platform %s (%s): %+v", action.Platform, errText, action)
		}
	}
}

func reachable(forges map[string]*Forge, roots []string) map[string]bool {
	parents := map[string][]string{}
	for _, f := range forges {
		for sha, ps := range f.Parents() {
			parents[sha] = ps
		}
	}
	seen := map[string]bool{}
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		stack = append(stack, parents[sha]...)
	}
	return seen
}

func lostCount(forges map[string]*Forge) int {
	universeSet := map[string]bool{}
	aliveSet := map[string]bool{}
	for _, f := range forges {
		for _, sha := range f.AllSHAs() {
			universeSet[sha] = true
		}
		for _, sha := range f.Refs() {
			aliveSet[sha] = true
		}
	}
	universe := keys(universeSet)
	alive := keys(aliveSet)
	reachableUniverse := reachable(forges, universe)
	reachableAlive := reachable(forges, alive)
	lost := 0
	for sha := range reachableUniverse {
		if !reachableAlive[sha] {
			lost++
		}
	}
	return lost
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func profileMatrix() []Profile {
	var out []Profile
	for _, force := range []bool{false, true} {
		for _, del := range []bool{false, true} {
			for _, create := range []string{"explicit", "push-create", "none"} {
				for _, listErr := range []bool{false, true} {
					for _, reserved := range []string{"none", "pull"} {
						p := Profile{
							RejectForce: force, RejectDelete: del, ListError: listErr,
							RepoCreate:     create == "explicit",
							PushCreateRepo: create == "push-create",
						}
						if reserved == "pull" {
							p.Reserved = []string{"refs/pull/*"}
						}
						p.Name = fmt.Sprintf("f%v-d%v-%s-l%v-%s", force, del, create, listErr, reserved)
						out = append(out, p)
					}
				}
			}
		}
	}
	return out
}

func TestHostileMatrix(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const now = int64(1759000000)
	for pi, profile := range profileMatrix() {
		profile := profile
		t.Run(profile.Name, func(t *testing.T) {
			for _, sc := range scenarios() {
				sc := sc
				t.Run(sc.name, func(t *testing.T) {
					a, b := seededForges(profile)
					sc.seed(a, b)
					forges := map[string]*Forge{"a": a, "b": b}
					input := buildInput(sc, a, b, now)
					plan := reconcile.Plan(input)

					assertCapabilities(t, plan, forges)
					assertNoActionsToUnknown(t, plan, input.Unknown)
					if sc.name == "list-error" {
						for _, action := range plan.Actions {
							if action.Kind == reconcile.ActionDeleteRef {
								t.Fatalf("deletion inferred from a listing error: %+v", action)
							}
						}
					}
					if sc.name == "namespace" {
						for _, action := range plan.Actions {
							if action.Platform == "b" && action.Ref == "refs/pull/1/head" {
								t.Fatalf("wrote to a reserved namespace: %+v", action)
							}
						}
					}

					slug := fmt.Sprintf("alice/m%d-%s", pi, sc.name)
					repo, err := store.EnsureRepo(ctx, slug, "default", 1)
					if err != nil {
						t.Fatal(err)
					}
					exec := execute.New(store, Cluster{Forges: forges}, repo.ID, slug)
					exec.Now = func() time.Time { return time.Unix(now, 0) }
					exec.Sleep = func(time.Duration) {}
					if _, err := exec.Run(ctx, plan); err != nil {
						t.Fatalf("executor refused its own plan: %v\nplan: %+v", err, plan.Actions)
					}
					if lost := lostCount(forges); lost != 0 {
						t.Fatalf("lost commits = %d after %s", lost, sc.name)
					}
				})
			}
		})
	}
}

func TestKillAtEveryJournalledStepRecoverable(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	profile := Profile{Name: "x", RejectForce: false, RejectDelete: false}
	const now = int64(1759000000)
	sc := scenario{
		name: "authority", authority: "a", divergence: config.DivergenceForceFromAuthority,
		seed: func(a, b *Forge) {
			a.SeedRef("refs/heads/main", s3)
			b.SeedRef("refs/heads/main", s4)
		},
		relations: func(r reconcile.Relations) { r.SetUnrelated(s3, s4) },
		bases:     map[string]string{"refs/heads/main": s2},
	}
	a, b := seededForges(profile)
	sc.seed(a, b)
	plan := reconcile.Plan(buildInput(sc, a, b, now))
	if len(plan.Actions) < 2 {
		t.Fatalf("expected a snapshot and a force, got %+v", plan.Actions)
	}
	actions := plan.Actions
	for k := 0; k <= len(actions); k++ {
		k := k
		t.Run(fmt.Sprintf("after-%d", k), func(t *testing.T) {
			a, b := seededForges(profile)
			sc.seed(a, b)
			forges := map[string]*Forge{"a": a, "b": b}
			slug := fmt.Sprintf("alice/crash-%d", k)
			repo, err := store.EnsureRepo(ctx, slug, "default", 1)
			if err != nil {
				t.Fatal(err)
			}
			groupID := fmt.Sprintf("group-%d", k)
			payloads := make([]state.JournalAction, len(actions))
			for i, action := range actions {
				data, _ := json.Marshal(action)
				payloads[i] = state.JournalAction{
					Action: string(action.Kind), Platform: action.Platform, Ref: action.Ref,
					FromSHA: action.FromSHA, ToSHA: action.ToSHA, Payload: string(data),
				}
			}
			ids, err := store.BeginGroup(ctx, repo.ID, groupID, payloads, 100)
			if err != nil {
				t.Fatal(err)
			}
			// Model a crash after k actions already happened and were journalled.
			for i := 0; i < k; i++ {
				if err := applyDirect(ctx, forges, actions[i], now); err != nil {
					t.Fatalf("prefix action %d: %v", i, err)
				}
				if err := store.MarkAction(ctx, ids[i], "done", "", 1, 101); err != nil {
					t.Fatal(err)
				}
			}
			exec := execute.New(store, Cluster{Forges: forges}, repo.ID, slug)
			exec.Now = func() time.Time { return time.Unix(now, 0) }
			exec.Sleep = func(time.Duration) {}
			if _, err := exec.Run(ctx, reconcile.Result{}); err != nil {
				t.Fatalf("recovery after %d steps: %v", k, err)
			}
			if got := forges["b"].refs["refs/heads/main"]; got != s3 {
				t.Fatalf("branch after recovery = %q, want %q", got, s3)
			}
			entries, _ := store.GroupActions(ctx, groupID)
			for _, entry := range entries {
				if entry.Status != "done" && entry.Status != "skipped" {
					t.Fatalf("journal not settled after recovery: %+v", entry)
				}
			}
			if lost := lostCount(forges); lost != 0 {
				t.Fatalf("lost commits = %d after recovery", lost)
			}
		})
	}
}

func applyDirect(ctx context.Context, forges map[string]*Forge, a reconcile.Action, now int64) error {
	f := forges[a.Platform]
	switch a.Kind {
	case reconcile.ActionSnapshot:
		_, err := f.PushSnapshot(ctx, a.Platform, "alice/x", a.Ref, a.FromSHA, now)
		return err
	case reconcile.ActionForcePush:
		return f.Push(ctx, a.Platform, "alice/x", a.Ref, a.ToSHA, true, a.LeaseSHA)
	case reconcile.ActionPush:
		return f.Push(ctx, a.Platform, "alice/x", a.Ref, a.ToSHA, false, "")
	case reconcile.ActionDeleteRef:
		return f.DeleteRef(ctx, a.Platform, "alice/x", a.Ref)
	case reconcile.ActionCreateRepo:
		return f.CreateRepo(ctx, a.Platform, "alice/x", a.SourceSHA)
	case reconcile.ActionUnprotect:
		return f.Unprotect(ctx, a.Platform, "alice/x", a.Ref)
	case reconcile.ActionReprotect:
		return f.Reprotect(ctx, a.Platform, "alice/x", a.Ref)
	case reconcile.ActionMergeAttempt:
		_, _, err := f.MergeAttempt(ctx, a.Platform, "alice/x", a.Ref, a.FromSHA, a.Ours, a.Theirs, now)
		return err
	}
	return nil
}
