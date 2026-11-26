package execute

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/cache"
	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/forge"
	"github.com/NoahIsARider/nuncio/internal/gitx"
	"github.com/NoahIsARider/nuncio/internal/reconcile"
)

// Binding is one platform's coordinates and adapter for a logical repo.
type Binding struct {
	Platform string
	Adapter  forge.Adapter
	Owner    string
	Name     string
}

// GitRemote wires the cache and adapters into an executable Remote.
type GitRemote struct {
	Cache    *cache.Repo
	Slug     string
	Bindings map[string]Binding
	Identity config.Identity
}

func (g *GitRemote) binding(platform string) (Binding, error) {
	b, ok := g.Bindings[platform]
	if !ok || b.Adapter == nil {
		return Binding{}, fmt.Errorf("platform %q is not configured for %s", platform, g.Slug)
	}
	return b, nil
}

func (g *GitRemote) url(b Binding) string {
	return b.Adapter.GitURL(b.Owner, b.Name)
}

// ReadRef reads one ref's tip from the platform.
func (g *GitRemote) ReadRef(ctx context.Context, platform, repoSlug, ref string) (string, error) {
	b, err := g.binding(platform)
	if err != nil {
		return "", err
	}
	refs, err := g.Cache.Git.LsRemote(ctx, g.url(b), b.Adapter.GitEnv(), ref)
	if err != nil {
		return "", wrapGitError(err)
	}
	if r, ok := refs[ref]; ok {
		return r.SHA, nil
	}
	// Some servers strip odd refs from an exact ls-remote; a second unfiltered
	// read is cheap and avoids a false absence.
	if strings.HasPrefix(ref, "refs/nuncio/") {
		all, err := g.Cache.Git.LsRemote(ctx, g.url(b), b.Adapter.GitEnv())
		if err != nil {
			return "", wrapGitError(err)
		}
		return all[ref].SHA, nil
	}
	return "", nil
}

// RepoExists asks the platform API, falling back to "true" for plain git.
func (g *GitRemote) RepoExists(ctx context.Context, platform, repoSlug string) (bool, error) {
	b, err := g.binding(platform)
	if err != nil {
		return false, err
	}
	_, err = b.Adapter.GetRepo(ctx, b.Owner, b.Name)
	if err == nil {
		return true, nil
	}
	if forge.IsNotFound(err) {
		return false, nil
	}
	if errors.Is(err, forge.ErrUnsupported) {
		return true, nil
	}
	return false, err
}

// CreateRepo creates a repository using the platform API.
func (g *GitRemote) CreateRepo(ctx context.Context, platform, repoSlug, source string) error {
	b, err := g.binding(platform)
	if err != nil {
		return err
	}
	if !b.Adapter.Caps().RepoCreate {
		return fmt.Errorf("platform %s cannot create repositories", platform)
	}
	_, err = b.Adapter.CreateRepo(ctx, forge.Repo{
		Owner: b.Owner, Name: b.Name, CloneURL: source,
		DefaultBranch: "main",
	})
	return err
}

// Push pushes one refspec from the cache and checks LFS support before claiming
// success.
func (g *GitRemote) Push(ctx context.Context, platform, repoSlug, ref, sha string, force bool, leaseSHA string) error {
	b, err := g.binding(platform)
	if err != nil {
		return err
	}
	if err := g.ensureLFS(ctx, b, sha); err != nil {
		return err
	}
	refspec := sha + ":" + ref
	if force {
		refspec = "+" + refspec
	}
	opts := gitx.PushOptions{ForceWithLease: ""}
	if force && leaseSHA != "" {
		opts.ForceWithLease = ref + ":" + leaseSHA
	} else if force {
		opts.Force = true
	}
	if err := g.Cache.Git.Push(ctx, g.url(b), b.Adapter.GitEnv(), refspec, opts); err != nil {
		return wrapGitError(err)
	}
	if err := g.pushLFS(ctx, b); err != nil {
		return err
	}
	return nil
}

// PushSnapshot writes a permanent snapshot ref.
func (g *GitRemote) PushSnapshot(ctx context.Context, platform, repoSlug, ref, sha string, ts int64) (string, error) {
	b, err := g.binding(platform)
	if err != nil {
		return "", err
	}
	tag := reconcile.SnapshotRef(ref, ts)
	if err := g.ensureLFS(ctx, b, sha); err != nil {
		return "", err
	}
	if err := g.Cache.Git.Push(ctx, g.url(b), b.Adapter.GitEnv(), sha+":"+tag, gitx.PushOptions{}); err != nil {
		return "", wrapGitError(err)
	}
	if err := g.pushLFS(ctx, b); err != nil {
		return "", err
	}
	return tag, nil
}

// DeleteRef deletes one ref.
func (g *GitRemote) DeleteRef(ctx context.Context, platform, repoSlug, ref string) error {
	b, err := g.binding(platform)
	if err != nil {
		return err
	}
	if !b.Adapter.Caps().GitRefDelete {
		return fmt.Errorf("platform %s cannot delete refs", platform)
	}
	if err := g.Cache.Git.Push(ctx, g.url(b), b.Adapter.GitEnv(), ":"+ref, gitx.PushOptions{}); err != nil {
		return wrapGitError(err)
	}
	return nil
}

// MergeAttempt creates a deterministic merge commit and pushes it to the
// namespaced merge ref. The branch itself is untouched.
func (g *GitRemote) MergeAttempt(ctx context.Context, platform, repoSlug, ref, base, ours, theirs string, ts int64) (string, string, error) {
	b, err := g.binding(platform)
	if err != nil {
		return "", "", err
	}
	if ours == "" || theirs == "" {
		return "", "", fmt.Errorf("merge attempt needs two tips")
	}
	sha, _, err := g.Cache.CreateMergeCommit(ctx, base, ours, theirs, g.Identity.Name, g.Identity.Email, ts)
	if err != nil {
		return "", "", err
	}
	mergeRef := reconcile.MergeRef(ref, ts)
	if err := g.Cache.Git.Push(ctx, g.url(b), b.Adapter.GitEnv(), sha+":"+mergeRef, gitx.PushOptions{}); err != nil {
		return "", "", wrapGitError(err)
	}
	return mergeRef, sha, nil
}

// Unprotect temporarily removes branch protection on platforms where a force
// push would otherwise be rejected.
func (g *GitRemote) Unprotect(ctx context.Context, platform, repoSlug, ref string) error {
	b, err := g.binding(platform)
	if err != nil {
		return err
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if branch == ref {
		return nil
	}
	return b.Adapter.Unprotect(ctx, b.Owner, b.Name, branch)
}

// Reprotect restores branch protection.
func (g *GitRemote) Reprotect(ctx context.Context, platform, repoSlug, ref string) error {
	b, err := g.binding(platform)
	if err != nil {
		return err
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if branch == ref {
		return nil
	}
	return b.Adapter.Reprotect(ctx, b.Owner, b.Name, branch)
}

func (g *GitRemote) ensureLFS(ctx context.Context, b Binding, sha string) error {
	uses, err := g.Cache.Git.UsesLFS(ctx, sha)
	if err != nil || !uses {
		return err
	}
	if !b.Adapter.Caps().GitLFS {
		return fmt.Errorf("%s uses Git LFS but platform %s does not support it; refusing to claim the push succeeded", sha, b.Platform)
	}
	if !g.Cache.Git.HasLFS(ctx) {
		return fmt.Errorf("repository uses Git LFS but git-lfs is not installed; refusing to push %s", short(sha))
	}
	return nil
}

func (g *GitRemote) pushLFS(ctx context.Context, b Binding) error {
	if !b.Adapter.Caps().GitLFS {
		return nil
	}
	return g.Cache.Git.PushLFS(ctx, g.url(b), b.Adapter.GitEnv())
}

func wrapGitError(err error) error {
	if err == nil || IsRetryable(err) {
		return err
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"could not resolve host", "connection refused", "connection reset", "operation timed out",
		"timed out", "tls handshake", "unexpected disconnect", "rpc failed", "early eof",
		"the remote end hung up", "temporary failure", "502", "503", "504",
	} {
		if strings.Contains(msg, needle) {
			return &Retryable{Err: err}
		}
	}
	return err
}
