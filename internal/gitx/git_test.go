package gitx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := New(dir).Run(context.Background(), nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// tempDir works around Windows refusing to unlink read-only git objects during
// t.TempDir cleanup: make the tree writable first, in a cleanup registered
// after TempDir's own (so it runs before it).
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(path, 0o777)
			} else {
				_ = os.Chmod(path, 0o666)
			}
			return nil
		})
	})
	return dir
}

func initWorkRepo(t *testing.T) string {
	t.Helper()
	dir := tempDir(t)
	mustGit(t, dir, "init", "--quiet", "-b", "main")
	mustGit(t, dir, "config", "user.name", "Test")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	return dir
}

func commitFile(t *testing.T, dir, name, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", name)
	mustGit(t, dir, "commit", "--quiet", "-m", msg)
	return mustGit(t, dir, "rev-parse", "HEAD")
}

func TestPathNormalisation(t *testing.T) {
	if !LooksMSYS("/d/work/repo") {
		t.Error("/d/work/repo should look MSYS")
	}
	if !LooksMSYS("/mnt/d/work/repo") {
		t.Error("/mnt/d/work/repo should look MSYS")
	}
	if LooksMSYS("refs/heads/main") {
		t.Error("a ref is not a path")
	}
	if runtime.GOOS == "windows" {
		if got := NativePath("/d/work/repo"); got != "D:/work/repo" {
			t.Errorf("NativePath = %q", got)
		}
		if got := NativePath(`D:\work\repo`); got != "D:/work/repo" {
			t.Errorf("backslash path = %q", got)
		}
		if got := NativePath("/mnt/e/x"); got != "E:/x" {
			t.Errorf("mnt path = %q", got)
		}
		if _, err := RequireNative("/tmp/repo"); err == nil {
			t.Error("drive-less path accepted on Windows")
		}
	}
	if err := AssertNoMSYSArgs([]string{"push", "/d/work/repo.git"}); err == nil {
		t.Error("MSYS argument accepted")
	}
	if err := AssertNoMSYSArgs([]string{"push", "D:/work/repo.git", "refs/heads/main"}); err != nil {
		t.Errorf("native argument rejected: %v", err)
	}
	if got := FileURL(`D:\work\repo.git`); runtime.GOOS == "windows" && got != "file://D:/work/repo.git" {
		t.Errorf("FileURL = %q", got)
	}
}

func TestLsRemotePushVerifyDelete(t *testing.T) {
	ctx := context.Background()
	src := initWorkRepo(t)
	c1 := commitFile(t, src, "a.txt", "one\n", "one")

	bare := filepath.Join(tempDir(t), "remote.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	g := New(src)
	if err := g.Push(ctx, bare, nil, "refs/heads/main:refs/heads/main", PushOptions{}); err != nil {
		t.Fatalf("push: %v", err)
	}
	refs, err := g.LsRemote(ctx, bare, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := refs["refs/heads/main"].SHA; got != c1 {
		t.Fatalf("remote main = %s, want %s", got, c1)
	}

	c2 := commitFile(t, src, "b.txt", "two\n", "two")
	if err := g.Push(ctx, bare, nil, c2+":refs/heads/main", PushOptions{ForceWithLease: "refs/heads/main:" + c1}); err != nil {
		t.Fatalf("force-with-lease push: %v", err)
	}
	refs, _ = g.LsRemote(ctx, bare, nil)
	if refs["refs/heads/main"].SHA != c2 {
		t.Fatal("force push did not move ref")
	}

	// Delete and verify absence.
	if err := g.Push(ctx, bare, nil, ":refs/heads/main", PushOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	refs, _ = g.LsRemote(ctx, bare, nil)
	if _, ok := refs["refs/heads/main"]; ok {
		t.Fatal("ref still present after delete")
	}
}

func TestFetchAndAncestry(t *testing.T) {
	ctx := context.Background()
	src := initWorkRepo(t)
	c1 := commitFile(t, src, "a.txt", "one\n", "one")
	c2 := commitFile(t, src, "a.txt", "two\n", "two")
	mustGit(t, src, "checkout", "--quiet", "-b", "feature", c1)
	c3 := commitFile(t, src, "f.txt", "feature\n", "feature")
	mustGit(t, src, "checkout", "--quiet", "main")

	bare := filepath.Join(tempDir(t), "remote.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	sg := New(src)
	if err := sg.Push(ctx, bare, nil, "+refs/heads/main:refs/heads/main", PushOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := sg.Push(ctx, bare, nil, "+refs/heads/feature:refs/heads/feature", PushOptions{Force: true}); err != nil {
		t.Fatal(err)
	}

	cacheDir := filepath.Join(tempDir(t), "cache.git")
	if err := InitBare(ctx, cacheDir); err != nil {
		t.Fatal(err)
	}
	cache := New(cacheDir)
	if err := cache.FetchRefspecs(ctx, bare, nil,
		"+refs/heads/main:refs/nuncio/observed/local/main",
		"+refs/heads/feature:refs/nuncio/observed/local/feature"); err != nil {
		t.Fatal(err)
	}
	mainTip, err := cache.RevParse(ctx, "refs/nuncio/observed/local/main")
	if err != nil || mainTip != c2 {
		t.Fatalf("main tip = %q err=%v", mainTip, err)
	}
	if anc, known := cache.IsAncestor(ctx, c1, c2); !anc || !known {
		t.Error("c1 should be ancestor of c2")
	}
	if anc, known := cache.IsAncestor(ctx, c2, c3); anc || !known {
		t.Error("c2 should not be ancestor of c3")
	}
	base, err := cache.MergeBase(ctx, c2, c3)
	if err != nil {
		t.Fatal(err)
	}
	if base != c1 {
		t.Errorf("merge base = %s, want %s", base, c1)
	}
	reachable, err := cache.RevList(ctx, []string{c3})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reachable[c1]; !ok {
		t.Error("c1 not reachable from feature tip")
	}
	if _, ok := reachable[c2]; ok {
		t.Error("c2 should not be reachable from feature tip")
	}
}

func TestMergeTreePreservesConflicts(t *testing.T) {
	ctx := context.Background()
	src := initWorkRepo(t)
	base := commitFile(t, src, "a.txt", "base\n", "base")
	mustGit(t, src, "checkout", "--quiet", "-b", "ours")
	ours := commitFile(t, src, "a.txt", "ours\n", "ours")
	mustGit(t, src, "checkout", "--quiet", "-b", "theirs", base)
	theirs := commitFile(t, src, "a.txt", "theirs\n", "theirs")
	mustGit(t, src, "checkout", "--quiet", "ours")

	g := New(src)
	tree, conflicts, err := g.MergeTree(ctx, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if !conflicts {
		t.Fatal("expected conflicts")
	}
	if tree == "" {
		t.Fatal("no tree written")
	}
	sha1, conflicts, err := g.CreateMergeCommit(ctx, base, ours, theirs, "nuncio", "nuncio@localhost", 1759000000)
	if err != nil || !conflicts {
		t.Fatalf("create merge: %v conflicts=%v", err, conflicts)
	}
	sha2, _, err := g.CreateMergeCommit(ctx, base, ours, theirs, "nuncio", "nuncio@localhost", 1759000000)
	if err != nil {
		t.Fatal(err)
	}
	if sha1 != sha2 {
		t.Errorf("merge commit is not deterministic: %s vs %s", sha1, sha2)
	}
	parents := mustGit(t, src, "log", "-1", "--format=%P", sha1)
	if !strings.Contains(parents, ours) || !strings.Contains(parents, theirs) {
		t.Errorf("merge parents = %q", parents)
	}
}

func TestLooksLikeRewrite(t *testing.T) {
	ctx := context.Background()
	src := initWorkRepo(t)
	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		commitFile(t, src, "a.txt", msg+"\n", msg)
	}
	a := mustGit(t, src, "rev-parse", "HEAD")
	mustGit(t, src, "checkout", "--quiet", "--orphan", "rewrite")
	// Recreate equivalent history with the same subjects and different SHAs.
	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		commitFile(t, src, "a.txt", msg+"-rewritten\n", msg)
	}
	b := mustGit(t, src, "rev-parse", "HEAD")
	mustGit(t, src, "checkout", "--quiet", "main")
	if a == b {
		t.Fatal("test setup produced identical tips")
	}
	ok, err := New(src).LooksLikeRewrite(ctx, a, b, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("history with shared subjects should be classified as a rewrite")
	}
}

func TestLFSAttributeDetection(t *testing.T) {
	ctx := context.Background()
	src := initWorkRepo(t)
	if err := os.WriteFile(filepath.Join(src, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", ".gitattributes")
	mustGit(t, src, "commit", "--quiet", "-m", "lfs attributes")
	g := New(src)
	uses, err := g.UsesLFS(ctx, "HEAD")
	if err != nil || !uses {
		t.Fatalf("UsesLFS = %v, %v", uses, err)
	}
	// HasLFS depends on the host; report it without failing.
	_ = g.HasLFS(ctx)
}

func TestPushOptionsAndNoMirror(t *testing.T) {
	ctx := context.Background()
	src := initWorkRepo(t)
	commitFile(t, src, "a.txt", "one\n", "one")
	bare := filepath.Join(tempDir(t), "remote.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	mustGit(t, bare, "config", "receive.advertisePushOptions", "true")
	g := New(src)
	if err := g.Push(ctx, bare, nil, "refs/heads/main:refs/heads/main", PushOptions{
		PushOptions: []string{"skip-ci"},
		DryRun:      true,
	}); err != nil {
		t.Fatal(err)
	}
	refs, _ := g.LsRemote(ctx, bare, nil)
	if _, ok := refs["refs/heads/main"]; ok {
		t.Fatal("dry-run pushed a ref")
	}
}

func TestFetchSHAMakesObjectLocal(t *testing.T) {
	ctx := context.Background()
	src := initWorkRepo(t)
	c1 := commitFile(t, src, "a.txt", "one\n", "one")
	c2 := commitFile(t, src, "a.txt", "two\n", "two")
	bare := filepath.Join(tempDir(t), "remote.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := New(src).Push(ctx, bare, nil, "+refs/heads/main:refs/heads/main", PushOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(tempDir(t), "cache.git")
	if err := InitBare(ctx, cacheDir); err != nil {
		t.Fatal(err)
	}
	cache := New(cacheDir)
	if cache.HasObject(ctx, c1) {
		t.Fatal("fresh cache should not have c1")
	}
	if err := cache.FetchSHA(ctx, bare, nil, c1); err != nil {
		t.Fatal(err)
	}
	if !cache.HasObject(ctx, c1) {
		t.Fatal("fetch did not bring c1")
	}
	if cache.HasObject(ctx, c2) {
		t.Fatal("fetching an ancestor must not bring its descendants")
	}
	if err := cache.FetchSHA(ctx, bare, nil, c2); err != nil {
		t.Fatal(err)
	}
	if !cache.HasObject(ctx, c2) {
		t.Fatal("fetch did not bring c2")
	}
}

func TestUsesLFSWithoutAttributes(t *testing.T) {
	src := initWorkRepo(t)
	commitFile(t, src, "a.txt", "one\n", "one")
	uses, err := New(src).UsesLFS(context.Background(), "HEAD")
	if err != nil || uses {
		t.Fatalf("UsesLFS = %v, %v", uses, err)
	}
}
