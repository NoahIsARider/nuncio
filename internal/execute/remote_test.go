package execute

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoahIsARider/nuncio/internal/cache"
	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/forge"
	"github.com/NoahIsARider/nuncio/internal/gitx"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.New(dir).Run(context.Background(), nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func writableTemp(t *testing.T) string {
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

func TestGitRemoteEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := writableTemp(t)

	// Source history.
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "--quiet", "-b", "main")
	gitRun(t, work, "config", "user.name", "Test")
	gitRun(t, work, "config", "user.email", "test@example.com")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644)
	gitRun(t, work, "add", "a.txt")
	gitRun(t, work, "commit", "--quiet", "-m", "one")
	c1 := gitRun(t, work, "rev-parse", "HEAD")

	// Two plain-git remotes with the same layout the adapter expects.
	baseA := filepath.Join(root, "forge-a")
	baseB := filepath.Join(root, "forge-b")
	bareA := filepath.Join(baseA, "alice", "example.git")
	bareB := filepath.Join(baseB, "alice", "example.git")
	if err := os.MkdirAll(filepath.Dir(bareA), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bareB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := gitx.InitBare(ctx, bareA); err != nil {
		t.Fatal(err)
	}
	if err := gitx.InitBare(ctx, bareB); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "push", "--quiet", gitx.FileURL(bareA), "main:refs/heads/main")

	// Cache contains the observed history.
	repo, err := cache.New(filepath.Join(root, "cache")).Open(ctx, "alice/example")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FetchRefs(ctx, "a", gitx.FileURL(bareA), nil, []string{"refs/heads/main"}); err != nil {
		t.Fatal(err)
	}

	adapterA, err := forge.New(ctx, "a", config.Platform{Kind: "git", Git: gitx.FileURL(baseA)}, forge.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	adapterB, err := forge.New(ctx, "b", config.Platform{Kind: "git", Git: gitx.FileURL(baseB)}, forge.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	remote := &GitRemote{
		Cache: repo, Slug: "alice/example",
		Bindings: map[string]Binding{
			"a": {Platform: "a", Adapter: adapterA, Owner: "alice", Name: "example"},
			"b": {Platform: "b", Adapter: adapterB, Owner: "alice", Name: "example"},
		},
		Identity: config.Identity{Name: "nuncio", Email: "nuncio@localhost"},
	}

	got, err := remote.ReadRef(ctx, "a", "alice/example", "refs/heads/main")
	if err != nil || got != c1 {
		t.Fatalf("read = %q, %v", got, err)
	}
	if got, _ := remote.ReadRef(ctx, "b", "alice/example", "refs/heads/main"); got != "" {
		t.Fatalf("b should be empty, got %q", got)
	}
	if err := remote.Push(ctx, "b", "alice/example", "refs/heads/main", c1, false, ""); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got, _ := remote.ReadRef(ctx, "b", "alice/example", "refs/heads/main"); got != c1 {
		t.Fatalf("b main = %q", got)
	}

	tag, err := remote.PushSnapshot(ctx, "a", "alice/example", "refs/heads/main", c1, 1759000000)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := remote.ReadRef(ctx, "a", "alice/example", tag); got != c1 {
		t.Fatalf("snapshot %s = %q", tag, got)
	}
	if err := remote.DeleteRef(ctx, "a", "alice/example", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if got, _ := remote.ReadRef(ctx, "a", "alice/example", "refs/heads/main"); got != "" {
		t.Fatalf("delete left %q", got)
	}
	// The snapshot survives the branch deletion, so no object is orphaned.
	if got, _ := remote.ReadRef(ctx, "a", "alice/example", tag); got != c1 {
		t.Fatal("snapshot disappeared")
	}
}

func TestGitRemoteRefusesUnsupportedLFS(t *testing.T) {
	// A platform whose caps say no LFS must not be told a push succeeded.
	ctx := context.Background()
	root := writableTemp(t)
	work := filepath.Join(root, "work")
	os.MkdirAll(work, 0o755)
	gitRun(t, work, "init", "--quiet", "-b", "main")
	gitRun(t, work, "config", "user.name", "Test")
	gitRun(t, work, "config", "user.email", "test@example.com")
	os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644)
	gitRun(t, work, "add", ".gitattributes")
	gitRun(t, work, "commit", "--quiet", "-m", "lfs")
	c1 := gitRun(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(root, "forge", "alice", "example.git")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	if err := gitx.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	repo, err := cache.New(filepath.Join(root, "cache")).Open(ctx, "alice/example")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureObjects(ctx, gitx.FileURL(bare), nil, []string{c1}); err != nil {
		// No remote objects yet; fetch from the work repo instead.
		if err := repo.EnsureObjects(ctx, work, nil, []string{c1}); err != nil {
			t.Fatal(err)
		}
	}
	adapter, _ := forge.New(ctx, "srht", config.Platform{Kind: "git", Git: gitx.FileURL(filepath.Join(root, "forge"))}, forge.Auth{})
	remote := &GitRemote{Cache: repo, Slug: "alice/example", Bindings: map[string]Binding{
		"srht": {Platform: "srht", Adapter: adapter, Owner: "alice", Name: "example"},
	}}
	err = remote.Push(ctx, "srht", "alice/example", "refs/heads/main", c1, false, "")
	if err == nil || !strings.Contains(err.Error(), "LFS") {
		t.Fatalf("expected a loud LFS refusal, got %v", err)
	}
}
