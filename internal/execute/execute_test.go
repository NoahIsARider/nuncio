package execute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/state"
)

type fakeRemote struct {
	refs      map[string]string
	exists    map[string]bool
	ops       []string
	calls     map[string]int
	failFn    func(op string, call int) error
	corrupt   bool
	mergeSHAs map[string]string
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{refs: map[string]string{}, exists: map[string]bool{}, calls: map[string]int{}, mergeSHAs: map[string]string{}}
}

func key(platform, ref string) string { return platform + "|" + ref }

func (f *fakeRemote) enter(op string) error {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[op]++
	if f.failFn != nil {
		return f.failFn(op, f.calls[op])
	}
	return nil
}

func (f *fakeRemote) ReadRef(_ context.Context, platform, repo, ref string) (string, error) {
	if err := f.enter("read"); err != nil {
		return "", err
	}
	return f.refs[key(platform, ref)], nil
}

func (f *fakeRemote) RepoExists(_ context.Context, platform, repo string) (bool, error) {
	if err := f.enter("repo-exists"); err != nil {
		return false, err
	}
	if v, ok := f.exists[platform]; ok {
		return v, nil
	}
	return true, nil
}

func (f *fakeRemote) CreateRepo(_ context.Context, platform, repo, source string) error {
	if err := f.enter("create-repo"); err != nil {
		return err
	}
	f.exists[platform] = true
	f.ops = append(f.ops, "create-repo:"+platform)
	return nil
}

func (f *fakeRemote) Push(_ context.Context, platform, repo, ref, sha string, force bool, lease string) error {
	op := "push"
	if force {
		op = "force-push"
	}
	if err := f.enter(op); err != nil {
		return err
	}
	if force && lease != "" {
		if got := f.refs[key(platform, ref)]; got != lease {
			return fmt.Errorf("lease mismatch: have %s want %s", got, lease)
		}
	}
	if f.corrupt {
		f.refs[key(platform, ref)] = "corrupted"
	} else {
		f.refs[key(platform, ref)] = sha
	}
	f.ops = append(f.ops, op+":"+platform+":"+ref)
	return nil
}

func (f *fakeRemote) PushSnapshot(_ context.Context, platform, repo, ref, sha string, ts int64) (string, error) {
	if err := f.enter("snapshot"); err != nil {
		return "", err
	}
	tag := reconcile.SnapshotRef(ref, ts)
	f.refs[key(platform, tag)] = sha
	f.ops = append(f.ops, "snapshot:"+platform+":"+ref)
	return tag, nil
}

func (f *fakeRemote) DeleteRef(_ context.Context, platform, repo, ref string) error {
	if err := f.enter("delete"); err != nil {
		return err
	}
	delete(f.refs, key(platform, ref))
	f.ops = append(f.ops, "delete:"+platform+":"+ref)
	return nil
}

func (f *fakeRemote) MergeAttempt(_ context.Context, platform, repo, ref, base, ours, theirs string, ts int64) (string, string, error) {
	if err := f.enter("merge"); err != nil {
		return "", "", err
	}
	sha := "merge-" + base
	mergeRef := reconcile.MergeRef(ref, ts)
	f.refs[key(platform, mergeRef)] = sha
	f.ops = append(f.ops, "merge:"+platform+":"+ref)
	return mergeRef, sha, nil
}

func (f *fakeRemote) Unprotect(_ context.Context, platform, repo, ref string) error {
	if err := f.enter("unprotect"); err != nil {
		return err
	}
	f.ops = append(f.ops, "unprotect:"+platform)
	return nil
}

func (f *fakeRemote) Reprotect(_ context.Context, platform, repo, ref string) error {
	if err := f.enter("reprotect"); err != nil {
		return err
	}
	f.ops = append(f.ops, "reprotect:"+platform)
	return nil
}

func newStore(t *testing.T) (*state.Store, int64) {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r, err := s.EnsureRepo(context.Background(), "alice/example", "default", 1)
	if err != nil {
		t.Fatal(err)
	}
	return s, r.ID
}

func newExec(t *testing.T, store *state.Store, repoID int64, remote Remote) *Executor {
	t.Helper()
	e := New(store, remote, repoID, "alice/example")
	e.Sleep = func(time.Duration) {}
	e.Now = func() time.Time { return time.Unix(1759000000, 0) }
	return e
}

func planOf(actions ...reconcile.Action) reconcile.Result {
	ref := "refs/heads/main"
	if len(actions) > 0 && actions[0].Ref != "" {
		ref = actions[0].Ref
	}
	return reconcile.Result{
		Repo:    "alice/example",
		Results: []reconcile.RefResult{{Ref: ref, Actions: actions}},
		Actions: actions,
	}
}

func snapshotAction(ref, sha string) reconcile.Action {
	return reconcile.Action{Kind: reconcile.ActionSnapshot, Platform: "github", Ref: ref, FromSHA: sha, SnapshotRef: reconcile.SnapshotRef(ref, 1759000000)}
}

func TestDryRunTouchesNothing(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	e := newExec(t, store, id, fake)
	e.DryRun = true
	push := reconcile.Action{Kind: reconcile.ActionPush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa"}
	res, err := e.Run(context.Background(), planOf(push))
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.ops) != 0 {
		t.Fatalf("dry run made calls: %v", fake.ops)
	}
	if res.Planned != 1 {
		t.Fatalf("planned = %d", res.Planned)
	}
	pending, _ := store.PendingActions(context.Background(), id)
	if len(pending) != 0 {
		t.Fatalf("dry run journalled work: %+v", pending)
	}
}

func TestVerificationMismatchIsOperationalAndNotRetried(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.corrupt = true
	e := newExec(t, store, id, fake)
	push := reconcile.Action{Kind: reconcile.ActionPush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa"}
	_, err := e.Run(context.Background(), planOf(push))
	if err == nil {
		t.Fatal("expected verification failure")
	}
	if IsSafety(err) {
		t.Fatalf("verification mismatch must not be a safety refusal: %v", err)
	}
	if fake.calls["push"] != 1 {
		t.Fatalf("verification failure was retried: %d pushes", fake.calls["push"])
	}
}

func TestForceWithoutSnapshotIsRefused(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.refs[key("github", "refs/heads/main")] = "bbb"
	e := newExec(t, store, id, fake)
	force := reconcile.Action{Kind: reconcile.ActionForcePush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa", LeaseSHA: "bbb", FromSHA: "bbb"}
	_, err := e.Run(context.Background(), planOf(force))
	if !IsSafety(err) {
		t.Fatalf("expected safety refusal, got %v", err)
	}
	if fake.calls["force-push"] != 0 {
		t.Fatal("force pushed without a snapshot")
	}
}

func TestForceLeaseMismatchIsRefused(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.refs[key("github", "refs/heads/main")] = "ccc"
	e := newExec(t, store, id, fake)
	snap := snapshotAction("refs/heads/main", "bbb")
	force := reconcile.Action{Kind: reconcile.ActionForcePush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa", LeaseSHA: "bbb", FromSHA: "bbb"}
	_, err := e.Run(context.Background(), planOf(snap, force))
	if !IsSafety(err) {
		t.Fatalf("expected lease refusal, got %v", err)
	}
	if fake.calls["force-push"] != 0 {
		t.Fatal("force push attempted after the remote moved")
	}
}

func TestSnapshotThenForce(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.refs[key("github", "refs/heads/main")] = "bbb"
	e := newExec(t, store, id, fake)
	snap := snapshotAction("refs/heads/main", "bbb")
	force := reconcile.Action{Kind: reconcile.ActionForcePush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa", LeaseSHA: "bbb", FromSHA: "bbb"}
	res, err := e.Run(context.Background(), planOf(snap, force))
	if err != nil {
		t.Fatal(err)
	}
	if res.Executed != 2 {
		t.Fatalf("executed = %d", res.Executed)
	}
	if got := fake.refs[key("github", "refs/heads/main")]; got != "aaa" {
		t.Fatalf("branch = %s", got)
	}
	if got := fake.refs[key("github", reconcile.SnapshotRef("refs/heads/main", 1759000000))]; got != "bbb" {
		t.Fatalf("snapshot = %s", got)
	}
	if len(fake.ops) < 2 || fake.ops[0] != "snapshot:github:refs/heads/main" {
		t.Fatalf("ops = %v", fake.ops)
	}
}

func TestTransientRetry(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.failFn = func(op string, call int) error {
		if op == "push" && call <= 2 {
			return &Retryable{Err: errors.New("connection reset")}
		}
		return nil
	}
	e := newExec(t, store, id, fake)
	push := reconcile.Action{Kind: reconcile.ActionPush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa"}
	res, err := e.Run(context.Background(), planOf(push))
	if err != nil {
		t.Fatal(err)
	}
	if res.Executed != 1 || fake.calls["push"] != 3 {
		t.Fatalf("executed=%d pushes=%d", res.Executed, fake.calls["push"])
	}
}

func TestCreateRepoPrecedesPush(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.exists["srht"] = false
	e := newExec(t, store, id, fake)
	actions := []reconcile.Action{
		{Kind: reconcile.ActionCreateRepo, Platform: "srht", Ref: "refs/heads/main"},
		{Kind: reconcile.ActionPush, Platform: "srht", Ref: "refs/heads/main", ToSHA: "aaa"},
	}
	if _, err := e.Run(context.Background(), planOf(actions...)); err != nil {
		t.Fatal(err)
	}
	if len(fake.ops) != 2 || fake.ops[0] != "create-repo:srht" || fake.ops[1] != "push:srht:refs/heads/main" {
		t.Fatalf("ops = %v", fake.ops)
	}
}

func TestDeleteRequiresSnapshot(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.refs[key("github", "refs/heads/main")] = "aaa"
	e := newExec(t, store, id, fake)
	del := reconcile.Action{Kind: reconcile.ActionDeleteRef, Platform: "github", Ref: "refs/heads/main", FromSHA: "aaa"}
	if _, err := e.Run(context.Background(), planOf(del)); !IsSafety(err) {
		t.Fatalf("delete without snapshot = %v", err)
	}
	if _, ok := fake.refs[key("github", "refs/heads/main")]; !ok {
		t.Fatal("ref was deleted without a snapshot")
	}
	snap := snapshotAction("refs/heads/main", "aaa")
	if _, err := e.Run(context.Background(), planOf(snap, del)); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.refs[key("github", "refs/heads/main")]; ok {
		t.Fatal("ref still exists")
	}
}

func TestSecondRunIsIdempotent(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.refs[key("github", "refs/heads/main")] = "bbb"
	e := newExec(t, store, id, fake)
	actions := []reconcile.Action{
		snapshotAction("refs/heads/main", "bbb"),
		{Kind: reconcile.ActionForcePush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa", LeaseSHA: "bbb", FromSHA: "bbb"},
	}
	if _, err := e.Run(context.Background(), planOf(actions...)); err != nil {
		t.Fatal(err)
	}
	opsAfterFirst := len(fake.ops)
	res, err := e.Run(context.Background(), planOf(actions...))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 2 || res.Executed != 0 {
		t.Fatalf("second run: %+v", res)
	}
	if len(fake.ops) != opsAfterFirst {
		t.Fatalf("second run mutated again: %v", fake.ops[opsAfterFirst:])
	}
}

func TestCrashRecoveryReplaysUnfinishedGroup(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	fake.refs[key("github", "refs/heads/main")] = "bbb"
	ctx := context.Background()
	actions := []reconcile.Action{
		snapshotAction("refs/heads/main", "bbb"),
		{Kind: reconcile.ActionForcePush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa", LeaseSHA: "bbb", FromSHA: "bbb"},
	}
	payloads := make([]state.JournalAction, len(actions))
	for i, a := range actions {
		data, _ := json.Marshal(a)
		payloads[i] = state.JournalAction{Action: string(a.Kind), Platform: a.Platform, Ref: a.Ref, FromSHA: a.FromSHA, ToSHA: a.ToSHA, Payload: string(data)}
	}
	ids, err := store.BeginGroup(ctx, id, "crash-group", payloads, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash after the snapshot was written but before the force push.
	fake.refs[key("github", snapshotAction("refs/heads/main", "bbb").SnapshotRef)] = "bbb"
	if err := store.MarkAction(ctx, ids[0], "done", "", 1, 101); err != nil {
		t.Fatal(err)
	}
	e := newExec(t, store, id, fake)
	if _, err := e.Run(ctx, reconcile.Result{}); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if got := fake.refs[key("github", "refs/heads/main")]; got != "aaa" {
		t.Fatalf("branch after recovery = %s", got)
	}
	group, _ := store.GroupActions(ctx, "crash-group")
	for _, entry := range group {
		if entry.Status != "done" {
			t.Errorf("journal entry not done after recovery: %+v", entry)
		}
	}
}

func TestRecoveryRefusesDestructiveActionWithoutSnapshot(t *testing.T) {
	store, id := newStore(t)
	fake := newFakeRemote()
	ctx := context.Background()
	a := reconcile.Action{Kind: reconcile.ActionForcePush, Platform: "github", Ref: "refs/heads/main", ToSHA: "aaa", LeaseSHA: "bbb", FromSHA: "bbb"}
	data, _ := json.Marshal(a)
	if _, err := store.BeginGroup(ctx, id, "bad-group", []state.JournalAction{{
		Action: string(a.Kind), Platform: a.Platform, Ref: a.Ref, FromSHA: a.FromSHA, ToSHA: a.ToSHA, Payload: string(data),
	}}, 100); err != nil {
		t.Fatal(err)
	}
	e := newExec(t, store, id, fake)
	_, err := e.Run(ctx, reconcile.Result{})
	if !IsSafety(err) {
		t.Fatalf("expected safety refusal, got %v", err)
	}
	if fake.calls["force-push"] != 0 {
		t.Fatal("recovery force pushed without a snapshot")
	}
}

func TestValidateSnapshotsOrder(t *testing.T) {
	ref := "refs/heads/main"
	force := reconcile.Action{Kind: reconcile.ActionForcePush, Platform: "github", Ref: ref, FromSHA: "bbb"}
	if err := validateSnapshots([]reconcile.Action{force}); err == nil {
		t.Fatal("unsnapshotted force passed validation")
	}
	snap := reconcile.Action{Kind: reconcile.ActionSnapshot, Platform: "github", Ref: ref, FromSHA: "bbb"}
	if err := validateSnapshots([]reconcile.Action{snap, force}); err != nil {
		t.Fatal(err)
	}
}
