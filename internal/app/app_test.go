package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/gitx"
	"github.com/NoahIsARider/nuncio/internal/reconcile"
)

func gitOutput(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func makeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			_ = os.Chmod(path, 0o666)
		}
		return nil
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	root  string
	work  string
	bare  map[string]string // platform -> bare path
	cfg   *config.Config
	clock int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { makeWritable(root) })
	f := &fixture{root: root, work: filepath.Join(root, "work"), bare: map[string]string{}}
	for _, platform := range []string{"a", "b"} {
		bare := filepath.Join(root, platform, "alice", "example.git")
		if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, "", "init", "--bare", "--quiet", bare)
		f.bare[platform] = bare
	}
	gitRun(t, "", "init", "-b", "main", f.work)
	writeFile(t, filepath.Join(f.work, "README.md"), "one\n")
	gitRun(t, f.work, "add", ".")
	gitRun(t, f.work, "commit", "-m", "one")
	gitRun(t, f.work, "remote", "add", "origin", gitx.FileURL(f.bare["a"]))
	gitRun(t, f.work, "push", "origin", "main")

	platforms := []string{"a", "b"}
	none, park, ignore := config.AuthorityNone, config.DivergencePark, config.DeleteIgnore
	yes := true
	defaultPolicy := "default"
	f.cfg = &config.Config{
		Version: 1,
		State: config.State{
			Path:  filepath.Join(root, "state.db"),
			Cache: filepath.Join(root, "repos"),
		},
		Daemon:   config.Daemon{Listen: "", Interval: "1h", Concurrency: 2, DryRun: false},
		Identity: config.Identity{Name: "nuncio", Email: "nuncio@localhost"},
		Platforms: map[string]config.Platform{
			"a": {Kind: "git", Git: gitx.FileURL(filepath.Join(root, "a"))},
			"b": {Kind: "git", Git: gitx.FileURL(filepath.Join(root, "b"))},
		},
		Policy: map[string]config.Policy{
			"default": {
				Platforms: &platforms, Authority: &none, OnDivergence: &park,
				OnDelete: &ignore, Snapshot: &yes, Verify: &yes,
			},
		},
		Repos: map[string]config.Repo{
			"alice/example": {Policy: &defaultPolicy},
		},
	}
	f.cfg.ApplyDefaults()
	if err := f.cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	return f
}

func (f *fixture) open(t *testing.T) *App {
	t.Helper()
	a, err := Open(context.Background(), f.cfg, Options{
		Now: func() time.Time {
			f.clock += 60
			return time.Unix(f.clock, 0)
		},
	})
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func (f *fixture) tip(t *testing.T, platform, ref string) string {
	t.Helper()
	out, err := gitOutput(t, "", "--git-dir", f.bare[platform], "rev-parse", ref)
	if err != nil {
		return ""
	}
	return out
}

func countActions(run *Run, kind reconcile.ActionKind) int {
	n := 0
	for _, action := range run.Plan.Actions {
		if action.Kind == kind {
			n++
		}
	}
	return n
}

func TestSyncFastForwardAndVerify(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	ctx := context.Background()

	run, err := a.Sync(ctx, "alice/example", false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := countActions(run, reconcile.ActionPush); got != 1 {
		t.Fatalf("push actions = %d, want 1\n%+v", got, run.Plan.Actions)
	}
	ta, tb := f.tip(t, "a", "refs/heads/main"), f.tip(t, "b", "refs/heads/main")
	if ta == "" || ta != tb {
		t.Fatalf("tips diverged after sync: a=%s b=%s", ta, tb)
	}
	if !run.Lost.Complete || run.Lost.Count != 0 {
		t.Fatalf("lost = %+v, want complete 0", run.Lost)
	}
	if run.Findings() != 0 {
		t.Fatalf("findings = %d, want 0", run.Findings())
	}

	// A second run must be idempotent: no push, no journal surprise.
	again, err := a.Sync(ctx, "alice/example", false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := countActions(again, reconcile.ActionPush); got != 0 {
		t.Fatalf("second sync emitted %d pushes", got)
	}
	if !again.Lost.Complete || again.Lost.Count != 0 {
		t.Fatalf("second lost = %+v", again.Lost)
	}

	report, err := a.Store.LatestReport(ctx, mustRepoID(t, a, "alice/example"))
	if err != nil {
		t.Fatalf("latest report: %v", err)
	}
	if !strings.Contains(report.JSON, `"lost_commits": 0`) {
		t.Fatalf("report JSON does not prove zero lost: %s", report.JSON)
	}
}

func TestSyncDryRunMutatesNothing(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	ctx := context.Background()

	run, err := a.Sync(ctx, "alice/example", true)
	if err != nil {
		t.Fatalf("dry sync: %v", err)
	}
	if countActions(run, reconcile.ActionPush) != 1 {
		t.Fatalf("dry run should still plan one push: %+v", run.Plan.Actions)
	}
	if got := f.tip(t, "b", "refs/heads/main"); got != "" {
		t.Fatalf("dry run pushed to b: %s", got)
	}
	if !run.Lost.Complete || run.Lost.Count != 0 {
		t.Fatalf("lost = %+v", run.Lost)
	}
}

func TestDivergenceParkedWithSnapshotsAndZeroLost(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	ctx := context.Background()
	if _, err := a.Sync(ctx, "alice/example", false); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// Rewrite main on a to an unrelated root commit and force it. b still holds
	// the old tip, so this is a true divergence.
	gitRun(t, f.work, "checkout", "--orphan", "rewrite")
	gitRun(t, f.work, "rm", "-rf", ".")
	writeFile(t, filepath.Join(f.work, "OTHER.md"), "other\n")
	gitRun(t, f.work, "add", ".")
	gitRun(t, f.work, "commit", "-m", "other-root")
	gitRun(t, f.work, "push", "--force", "origin", "HEAD:main")

	run, err := a.Sync(ctx, "alice/example", false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(run.Plan.Results) != 1 {
		t.Fatalf("results = %d", len(run.Plan.Results))
	}
	res := run.Plan.Results[0]
	if res.Class != reconcile.ClassDivergence {
		t.Fatalf("class = %s, want divergence", res.Class)
	}
	if got := countActions(run, reconcile.ActionForcePush); got != 0 {
		t.Fatalf("park emitted %d force pushes", got)
	}
	if got := countActions(run, reconcile.ActionSnapshot); got != 2 {
		t.Fatalf("snapshots = %d, want 2 (one per divergent tip)", got)
	}
	if !run.Lost.Complete || run.Lost.Count != 0 {
		t.Fatalf("lost = %+v, want complete 0 proof", run.Lost)
	}

	for _, platform := range []string{"a", "b"} {
		out := gitRun(t, "", "--git-dir", f.bare[platform], "for-each-ref", "refs/nuncio/snapshot/")
		if !strings.Contains(out, "refs/nuncio/snapshot/main/") {
			t.Fatalf("%s has no snapshot ref:\n%s", platform, out)
		}
	}
}

func TestSnapshotCreateAndRestore(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	ctx := context.Background()
	if _, err := a.Sync(ctx, "alice/example", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	first := f.tip(t, "a", "refs/heads/main")

	snap, err := a.SnapshotCreate(ctx, "alice/example", "refs/heads/main", "a")
	if err != nil {
		t.Fatalf("snapshot create: %v", err)
	}
	if snap.SHA != first {
		t.Fatalf("snapshot sha = %s, want %s", snap.SHA, first)
	}

	// Advance both forges, then restore a to the snapshot.
	writeFile(t, filepath.Join(f.work, "README.md"), "two\n")
	gitRun(t, f.work, "add", ".")
	gitRun(t, f.work, "commit", "-m", "two")
	gitRun(t, f.work, "push", "origin", "main")
	gitRun(t, f.work, "push", gitx.FileURL(f.bare["b"]), "main")
	second := f.tip(t, "a", "refs/heads/main")
	if second == first {
		t.Fatal("second commit did not land")
	}

	if err := a.SnapshotRestore(ctx, "alice/example", snap.Tag, "refs/heads/main", "a"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := f.tip(t, "a", "refs/heads/main"); got != first {
		t.Fatalf("restored tip = %s, want %s", got, first)
	}
	snaps, err := a.SnapshotList(ctx, "alice/example")
	if err != nil {
		t.Fatalf("snapshot list: %v", err)
	}
	// The restore snapshotted the tip it replaced, so the "lost" tip is kept.
	found := false
	for _, s := range snaps {
		if s.SHA == second && s.Platform == "a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restore did not preserve the tip it overwrote: %+v", snaps)
	}
}

func TestLostRefusesToClaimZeroWhenObjectsAreMissing(t *testing.T) {
	f := newFixture(t)
	a := f.open(t)
	ctx := context.Background()
	if _, err := a.Sync(ctx, "alice/example", false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	repoID := mustRepoID(t, a, "alice/example")
	// Inject a SHA that no local object can satisfy, as a corrupted or pruned
	// cache would look.
	if err := a.Store.RecordObservation(ctx, repoID, "refs/heads/ghost", "a", strings.Repeat("ab", 20), true, 1, ""); err != nil {
		t.Fatal(err)
	}
	lost, _, _, err := a.Lost(ctx, "alice/example")
	if err != nil {
		t.Fatalf("lost: %v", err)
	}
	if lost.Complete {
		t.Fatalf("lost claimed a complete answer with a missing object: %+v", lost)
	}
	if len(lost.Missing) == 0 {
		t.Fatal("lost did not name the missing object")
	}
}

func mustRepoID(t *testing.T, a *App, slug string) int64 {
	t.Helper()
	row, err := a.Store.GetRepo(context.Background(), slug)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	return row.ID
}
