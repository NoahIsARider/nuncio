package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/execute"
	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/state"
)

// SnapshotList lists the snapshots nuncio has written for a repo.
func (a *App) SnapshotList(ctx context.Context, slug string) ([]state.Snapshot, error) {
	spec, err := a.Spec(ctx, slug)
	if err != nil {
		return nil, err
	}
	row, err := a.Store.EnsureRepo(ctx, spec.Slug, spec.Policy, a.Now().Unix())
	if err != nil {
		return nil, err
	}
	return a.Store.ListSnapshots(ctx, row.ID)
}

// SnapshotCreate writes the current tip of ref to a permanent snapshot ref on
// one platform. It is an explicit operator action and always executes; the
// snapshot ref is never overwritten.
func (a *App) SnapshotCreate(ctx context.Context, slug, ref, platform string) (state.Snapshot, error) {
	spec, err := a.Spec(ctx, slug)
	if err != nil {
		return state.Snapshot{}, err
	}
	prep, err := a.prepare(ctx, spec)
	if err != nil {
		return state.Snapshot{}, err
	}
	resolved, err := a.policyFor(spec)
	if err != nil {
		return state.Snapshot{}, err
	}
	if ref == "" {
		ref = "refs/heads/" + resolved.DefaultBranch
	}
	if !strings.HasPrefix(ref, "refs/") {
		return state.Snapshot{}, fmt.Errorf("ref %q must start with refs/", ref)
	}
	if platform == "" {
		platform = firstWithRef(prep.Obs, ref, spec.Platforms)
	}
	if platform == "" {
		return state.Snapshot{}, fmt.Errorf("ref %q exists on no platform of %s", ref, slug)
	}
	obs, ok := prep.Obs.Refs[ref][platform]
	if !ok || !obs.Present || obs.SHA == "" {
		return state.Snapshot{}, fmt.Errorf("ref %q is not present on %s", ref, platform)
	}
	if _, reserved := a.platformCaps(platform).Reserved(ref); reserved {
		return state.Snapshot{}, fmt.Errorf("%q is in a reserved namespace on %s", ref, platform)
	}
	now := a.Now().Unix()
	tag, err := prep.Remote.PushSnapshot(ctx, platform, slug, ref, obs.SHA, now)
	if err != nil {
		return state.Snapshot{}, err
	}
	got, err := prep.Remote.ReadRef(ctx, platform, slug, tag)
	if err != nil {
		return state.Snapshot{}, err
	}
	if got != obs.SHA {
		return state.Snapshot{}, fmt.Errorf("snapshot verification failed: %s has %q, want %s", tag, got, obs.SHA)
	}
	if err := a.Store.RecordSnapshot(ctx, prep.RepoID, platform, ref, obs.SHA, tag, now); err != nil {
		return state.Snapshot{}, err
	}
	return state.Snapshot{RepoID: prep.RepoID, Platform: platform, Ref: ref, SHA: obs.SHA, Tag: tag, CreatedAt: now}, nil
}

// SnapshotRestore moves a ref back to a recorded snapshot. The current tip is
// snapshotted first, so even this deliberate overwrite loses nothing.
func (a *App) SnapshotRestore(ctx context.Context, slug, tag, ref, platform string) error {
	spec, err := a.Spec(ctx, slug)
	if err != nil {
		return err
	}
	row, err := a.Store.EnsureRepo(ctx, spec.Slug, spec.Policy, a.Now().Unix())
	if err != nil {
		return err
	}
	snap, err := a.Store.FindSnapshot(ctx, row.ID, tag)
	if err != nil {
		return fmt.Errorf("snapshot %q is not recorded for %s: %w", tag, slug, err)
	}
	if ref == "" {
		ref = snap.Ref
	}
	if !strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("ref %q must start with refs/", ref)
	}
	if strings.HasPrefix(ref, "refs/tags/") != strings.HasPrefix(snap.Ref, "refs/tags/") {
		return fmt.Errorf("cannot restore a tag snapshot to a branch, or a branch snapshot to a tag")
	}
	if platform == "" {
		platform = snap.Platform
	}
	if !containsString(spec.Platforms, platform) {
		return fmt.Errorf("platform %q is not part of %s", platform, slug)
	}
	if _, reserved := a.platformCaps(platform).Reserved(ref); reserved {
		return fmt.Errorf("%q is in a reserved namespace on %s", ref, platform)
	}
	prep, err := a.prepare(ctx, spec)
	if err != nil {
		return err
	}
	current, err := prep.Remote.ReadRef(ctx, platform, slug, ref)
	if err != nil {
		return err
	}
	if current == snap.SHA {
		return nil
	}
	now := a.Now().Unix()
	actions := []reconcile.Action{}
	protected := a.platformCaps(platform).GitProtected && strings.HasPrefix(ref, "refs/heads/")
	if current == "" {
		actions = append(actions, reconcile.Action{
			Kind: reconcile.ActionPush, Platform: platform, Ref: ref,
			FromSHA: snap.SHA, ToSHA: snap.SHA, Reason: "restore snapshot to a missing ref",
		})
	} else {
		actions = append(actions, reconcile.Action{
			Kind: reconcile.ActionSnapshot, Platform: platform, Ref: ref,
			FromSHA: current, SnapshotRef: reconcile.SnapshotRef(ref, now),
			Reason: "before restoring " + tag,
		})
		if protected {
			actions = append(actions, reconcile.Action{Kind: reconcile.ActionUnprotect, Platform: platform, Ref: ref})
		}
		actions = append(actions, reconcile.Action{
			Kind: reconcile.ActionForcePush, Platform: platform, Ref: ref,
			FromSHA: current, ToSHA: snap.SHA, LeaseSHA: current,
			Reason: "restore snapshot " + tag,
		})
		if protected {
			actions = append(actions, reconcile.Action{Kind: reconcile.ActionReprotect, Platform: platform, Ref: ref})
		}
	}
	plan := reconcile.Result{
		Repo: slug,
		Results: []reconcile.RefResult{{
			Ref: ref, Class: reconcile.ClassHistoryRewrite, Actions: actions,
		}},
		Actions: actions,
	}
	executor := execute.New(a.Store, prep.Remote, prep.RepoID, slug)
	executor.Now = a.Now
	executor.Log = a.Logf
	_, err = executor.Run(ctx, plan)
	return err
}

func firstWithRef(obs Observation, ref string, platforms []string) string {
	ordered := append([]string(nil), platforms...)
	sort.Strings(ordered)
	for _, platform := range ordered {
		if o, ok := obs.Refs[ref][platform]; ok && o.Present && o.SHA != "" {
			return platform
		}
	}
	return ""
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// reservedFor is used by doctor output.
func reservedFor(c caps.Capabilities) string {
	if len(c.ReservedRefs) == 0 {
		return "-"
	}
	return strings.Join(c.ReservedRefs, " ")
}
