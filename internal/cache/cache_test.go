package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoahIsARider/nuncio/internal/gitx"
)

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

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.New(dir).Run(context.Background(), nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func workRepo(t *testing.T) string {
	dir := tempDir(t)
	git(t, dir, "init", "--quiet", "-b", "main")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "user.email", "test@example.com")
	return dir
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "--quiet", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

func pushAll(t *testing.T, src, bare string) {
	git(t, src, "push", "--quiet", bare, "+refs/heads/*:refs/heads/*")
}

func TestDirName(t *testing.T) {
	a := DirName("alice/example")
	b := DirName("alice/example")
	if a != b || !strings.HasSuffix(a, ".git") {
		t.Fatalf("DirName = %q / %q", a, b)
	}
	if DirName("alice/exa_mple") == DirName("alice/exa/mple") {
		t.Error("sanitisation collided")
	}
	if strings.ContainsAny(a, "/\\") {
		t.Errorf("DirName is not a single path element: %q", a)
	}
}

func TestFetchAndObserve(t *testing.T) {
	ctx := context.Background()
	src := workRepo(t)
	c1 := commit(t, src, "one")
	bare := filepath.Join(tempDir(t), "remote.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	pushAll(t, src, bare)

	c := New(filepath.Join(tempDir(t), "cache"))
	repo, err := c.Open(ctx, "alice/example")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FetchRefs(ctx, "github", bare, nil, []string{"refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	if got := repo.ObservedRef(ctx, "github", "refs/heads/main"); got != c1 {
		t.Fatalf("observed = %s, want %s", got, c1)
	}
	if !repo.HasAll(ctx, []string{c1}) {
		t.Fatal("object not local after fetch")
	}
}

func TestLostComputation(t *testing.T) {
	ctx := context.Background()
	src := workRepo(t)
	c1 := commit(t, src, "one")
	c2 := commit(t, src, "two")
	git(t, src, "checkout", "--quiet", "--orphan", "rewrite")
	git(t, src, "rm", "-rf", "--quiet", ".")
	d1 := commit(t, src, "one-rewritten")
	d2 := commit(t, src, "two-rewritten")
	git(t, src, "checkout", "--quiet", "main")

	bare := filepath.Join(tempDir(t), "remote.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	git(t, src, "push", "--quiet", bare, "+refs/heads/main:refs/heads/main", "+refs/heads/rewrite:refs/heads/rewrite")

	c := New(filepath.Join(tempDir(t), "cache"))
	repo, err := c.Open(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	universe := []string{c2, d2}
	if err := repo.EnsureObjects(ctx, bare, nil, universe); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Lost(ctx, universe, []string{d2})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete {
		t.Fatal("lost computation incomplete")
	}
	if got.Count != 2 {
		t.Fatalf("lost = %d (%v), want c1 and c2", got.Count, got.SHAs)
	}
	// A snapshot of the old tip makes the same history reachable again.
	got, err = repo.Lost(ctx, universe, []string{d2, c2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 {
		t.Fatalf("snapshot did not protect history: %v", got.SHAs)
	}
	_ = d1
	_ = c1
}

func TestLostRefusesWhenObjectsMissing(t *testing.T) {
	ctx := context.Background()
	c := New(filepath.Join(tempDir(t), "cache"))
	repo, err := c.Open(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	missing := "1234567890123456789012345678901234567890"
	res, err := repo.Lost(ctx, []string{missing}, nil)
	var missingErr *ErrMissingObjects
	if !errors.As(err, &missingErr) {
		t.Fatalf("expected ErrMissingObjects, got %v", err)
	}
	if res.Complete {
		t.Fatal("incomplete result marked complete")
	}
	if len(res.Missing) != 1 || res.Missing[0] != missing {
		t.Fatalf("missing = %v", res.Missing)
	}
}

func TestAncestryFacts(t *testing.T) {
	ctx := context.Background()
	src := workRepo(t)
	c1 := commit(t, src, "one")
	c2 := commit(t, src, "two")
	git(t, src, "checkout", "--quiet", "-b", "feature", c1)
	c3 := commit(t, src, "feature")
	git(t, src, "checkout", "--quiet", "main")
	bare := filepath.Join(tempDir(t), "remote.git")
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	git(t, src, "push", "--quiet", bare, "+refs/heads/*:refs/heads/*")
	repo, err := New(filepath.Join(tempDir(t), "cache")).Open(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureObjects(ctx, bare, nil, []string{c2, c3}); err != nil {
		t.Fatal(err)
	}
	facts := repo.Ancestry(ctx, []string{c2, c3})
	found := false
	for _, f := range facts {
		if f.A == c2 && f.B == c3 {
			found = true
			if !f.Known || f.Ancestor {
				t.Fatalf("c2..c3 fact = %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("no fact for c2/c3: %+v", facts)
	}
	base, err := repo.MergeBase(ctx, c2, c3)
	if err != nil || base != c1 {
		t.Fatalf("merge base = %q, %v", base, err)
	}
}
