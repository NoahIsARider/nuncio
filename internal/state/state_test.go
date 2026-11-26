package state

import (
	"context"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSchemaAndRepo(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v, err := s.SchemaVersion(ctx)
	if err != nil || v != 1 {
		t.Fatalf("schema version = %d, %v", v, err)
	}
	r, err := s.EnsureRepo(ctx, "alice/example", "default", 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.Slug != "alice/example" {
		t.Fatalf("repo = %+v", r)
	}
	again, err := s.EnsureRepo(ctx, "alice/example", "site", 200)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != r.ID || again.Policy != "site" {
		t.Fatalf("repo not updated in place: %+v", again)
	}
	repos, err := s.ListRepos(ctx)
	if err != nil || len(repos) != 1 {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	if _, err := s.GetRepo(ctx, "nope/nope"); err == nil {
		t.Error("missing repo did not error")
	}
}

func TestRepoPlatformLookup(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r, _ := s.EnsureRepo(ctx, "alice/example", "default", 1)
	if err := s.SetRepoPlatform(ctx, RepoPlatform{RepoID: r.ID, Platform: "github", Owner: "alice", Name: "example"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoPlatform(ctx, RepoPlatform{RepoID: r.ID, Platform: "codeberg", Owner: "Alice", Name: "Example", CreatedByNuncio: true}); err != nil {
		t.Fatal(err)
	}
	found, err := s.FindRepoByRemote(ctx, "github", "ALICE", "example")
	if err != nil || found.ID != r.ID {
		t.Fatalf("lookup = %+v, %v", found, err)
	}
	plats, err := s.RepoPlatforms(ctx, r.ID)
	if err != nil || len(plats) != 2 {
		t.Fatalf("platforms = %+v, %v", plats, err)
	}
}

func TestObservationHistoryAndUniverse(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r, _ := s.EnsureRepo(ctx, "a/b", "default", 1)
	if err := s.RecordObservation(ctx, r.ID, "refs/heads/main", "github", "aaa", true, 10, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation(ctx, r.ID, "refs/heads/main", "github", "aaa", false, 20, "pushed"); err != nil {
		t.Fatal(err)
	}
	states, err := s.LoadRefState(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := states["refs/heads/main"]["github"]
	if st.Present {
		t.Error("ref should be absent")
	}
	if st.SHA != "aaa" {
		t.Errorf("last known sha lost: %+v", st)
	}
	if st.LastAction != "pushed" {
		t.Errorf("last action = %q", st.LastAction)
	}
	if err := s.RecordObservation(ctx, r.ID, "refs/heads/main", "github", "bbb", true, 30, ""); err != nil {
		t.Fatal(err)
	}
	universe, err := s.UniverseSHAs(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(universe) != 2 || universe[0] != "aaa" || universe[1] != "bbb" {
		t.Fatalf("universe = %v", universe)
	}
	if err := s.MarkAbsent(ctx, r.ID, "refs/heads/main", "github", 40); err != nil {
		t.Fatal(err)
	}
	states, _ = s.LoadRefState(ctx, r.ID)
	if states["refs/heads/main"]["github"].Present {
		t.Error("MarkAbsent did not clear presence")
	}
}

func TestJournalReplay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r, _ := s.EnsureRepo(ctx, "a/b", "default", 1)
	ids, err := s.BeginGroup(ctx, r.ID, "grp-1", []JournalAction{
		{Action: "snapshot", Platform: "github", Ref: "refs/heads/main", FromSHA: "aaa"},
		{Action: "force-push", Platform: "codeberg", Ref: "refs/heads/main", ToSHA: "aaa", FromSHA: "bbb"},
	}, 100)
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	pending, err := s.PendingActions(ctx, r.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if err := s.MarkActionRunning(ctx, ids[0], 1, 101); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAction(ctx, ids[0], "done", "", 1, 102); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingActions(ctx, r.ID)
	if len(pending) != 1 || pending[0].Status != "planned" {
		t.Fatalf("pending after one done = %+v", pending)
	}
	group, err := s.GroupActions(ctx, "grp-1")
	if err != nil || len(group) != 2 {
		t.Fatalf("group = %+v, %v", group, err)
	}
	if group[0].Status != "done" || group[0].Attempts != 1 {
		t.Errorf("journal entry = %+v", group[0])
	}
	n, err := s.PruneJournal(ctx, 200)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
}

func TestSnapshotsAndPairsAndReports(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r, _ := s.EnsureRepo(ctx, "a/b", "default", 1)
	if err := s.RecordSnapshot(ctx, r.ID, "github", "refs/heads/main", "aaa", "refs/nuncio/snapshot/main/1", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSnapshot(ctx, r.ID, "github", "refs/heads/main", "aaa", "refs/nuncio/snapshot/main/1", 11); err != nil {
		t.Fatal(err)
	}
	snaps, err := s.ListSnapshots(ctx, r.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots = %+v, %v", snaps, err)
	}
	found, err := s.FindSnapshot(ctx, r.ID, "refs/nuncio/snapshot/main/1")
	if err != nil || found.SHA != "aaa" {
		t.Fatalf("find = %+v, %v", found, err)
	}
	shas, err := s.SnapshotSHAs(ctx, r.ID)
	if err != nil || len(shas) != 1 {
		t.Fatalf("snapshot shas = %v, %v", shas, err)
	}
	if err := s.RecordPair(ctx, Pair{RepoID: r.ID, RefA: "refs/heads/main", SHA_A: "aaa", RefB: "refs/heads/main", SHA_B: "bbb", Class: "divergence", DetectedAt: 5}); err != nil {
		t.Fatal(err)
	}
	pairs, err := s.ListPairs(ctx, r.ID)
	if err != nil || len(pairs) != 1 || pairs[0].SHA_A != "aaa" {
		t.Fatalf("pairs = %+v, %v", pairs, err)
	}
	if _, err := s.RecordReport(ctx, ReportRecord{RepoID: r.ID, StartedAt: 1, FinishedAt: 2, DryRun: true, Lost: 0, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.LatestReport(ctx, r.ID)
	if err != nil || rep.JSON != "{}" || !rep.DryRun {
		t.Fatalf("report = %+v, %v", rep, err)
	}
}

func TestWebhookDedupe(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	fresh, err := s.MarkWebhookSeen(ctx, "delivery-1", 10)
	if err != nil || !fresh {
		t.Fatalf("first = %v, %v", fresh, err)
	}
	again, err := s.MarkWebhookSeen(ctx, "delivery-1", 11)
	if err != nil || again {
		t.Fatalf("second = %v, %v", again, err)
	}
	empty, err := s.MarkWebhookSeen(ctx, "", 12)
	if err != nil || !empty {
		t.Fatalf("empty id should be accepted: %v, %v", empty, err)
	}
}
