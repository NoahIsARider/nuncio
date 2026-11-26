// Package fakeforge is a hostile in-memory forge used by the matrix tests. It
// enforces its declared capabilities, so a planner that emits an impossible
// action fails the test instead of silently producing a nice-looking report.
package fakeforge

import (
	"context"
	"fmt"
	"sync"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/reconcile"
)

// Profile describes a platform's limitations.
type Profile struct {
	Name           string
	RejectForce    bool
	RejectDelete   bool
	RepoCreate     bool
	PushCreateRepo bool
	Reserved       []string
	Protected      bool
	ListError      bool
	MissingRepo    bool
}

// Forge is one fake platform.
type Forge struct {
	Profile Profile
	Caps    caps.Capabilities

	mu        sync.Mutex
	refs      map[string]string
	repoExist bool
	allSHAs   map[string]bool
	parents   map[string][]string
	ops       []string
	pushCount int
}

// New builds a fake with capabilities derived from its profile.
func New(p Profile) *Forge {
	c := caps.Capabilities{
		GitPush: true, GitPushForce: !p.RejectForce, GitRefDelete: !p.RejectDelete,
		GitPushCreateRepo: p.PushCreateRepo, GitProtected: p.Protected,
		RepoCreate: p.RepoCreate, RepoDelete: true,
		MetaRead: true, MetaWrite: true,
		ReservedRefs: p.Reserved,
	}
	return &Forge{
		Profile: p, Caps: c, refs: map[string]string{}, repoExist: !p.MissingRepo,
		allSHAs: map[string]bool{}, parents: map[string][]string{},
	}
}

// AddCommit registers a commit and its parents for reachability checks. It does
// not put the commit in the "ever observed" universe; only refs do that.
func (f *Forge) AddCommit(sha string, parents ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parents[sha] = append([]string{}, parents...)
}

// SeedRef sets a ref without going through capability checks (initial state).
func (f *Forge) SeedRef(ref, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[ref] = sha
	f.allSHAs[sha] = true
}

// DeleteSeededRef removes a ref as if a human deleted it out of band.
func (f *Forge) DeleteSeededRef(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refs, ref)
}

// List returns the visible refs, or the configured outage.
func (f *Forge) List() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Profile.ListError {
		return nil, fmt.Errorf("%s: simulated listing outage", f.Profile.Name)
	}
	out := map[string]string{}
	for ref, sha := range f.refs {
		out[ref] = sha
	}
	return out, nil
}

// Refs returns a copy of every ref, including nuncio snapshots.
func (f *Forge) Refs() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for ref, sha := range f.refs {
		out[ref] = sha
	}
	return out
}

// AllSHAs returns every SHA ever placed in this forge.
func (f *Forge) AllSHAs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.allSHAs))
	for sha := range f.allSHAs {
		out = append(out, sha)
	}
	return out
}

// Parents exposes the commit graph.
func (f *Forge) Parents() map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for k, v := range f.parents {
		out[k] = append([]string{}, v...)
	}
	return out
}

// Ops lists mutations for debugging.
func (f *Forge) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.ops...)
}

func (f *Forge) record(op string) {
	f.ops = append(f.ops, op)
}

// isAncestorLocked checks the fake commit graph. The caller holds the lock.
func (f *Forge) isAncestorLocked(ancestor, descendant string) bool {
	stack := []string{descendant}
	seen := map[string]bool{}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if sha == ancestor {
			return true
		}
		if seen[sha] {
			continue
		}
		seen[sha] = true
		stack = append(stack, f.parents[sha]...)
	}
	return false
}

// ReadRef reads a ref.
func (f *Forge) ReadRef(_ context.Context, platform, repo, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refs[ref], nil
}

// RepoExists reports whether the repo exists.
func (f *Forge) RepoExists(context.Context, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoExist, nil
}

// CreateRepo creates the fake repo if the capability allows it.
func (f *Forge) CreateRepo(_ context.Context, platform, repo, source string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Caps.RepoCreate {
		return fmt.Errorf("%s: cannot create repositories", f.Profile.Name)
	}
	f.repoExist = true
	f.record("create-repo")
	return nil
}

// Push enforces the declared capabilities.
func (f *Forge) Push(_ context.Context, platform, repo, ref, sha string, force bool, lease string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if glob, reserved := f.Caps.Reserved(ref); reserved {
		return fmt.Errorf("%s: refuses %s (reserved %s)", f.Profile.Name, ref, glob)
	}
	if !f.Caps.GitPush {
		return fmt.Errorf("%s: refuses pushes", f.Profile.Name)
	}
	if force && !f.Caps.GitPushForce {
		return fmt.Errorf("%s: refuses force-pushes", f.Profile.Name)
	}
	if force && lease != "" && f.refs[ref] != lease {
		return fmt.Errorf("%s: lease mismatch on %s", f.Profile.Name, ref)
	}
	if !force && f.refs[ref] != "" && f.refs[ref] != sha && !f.isAncestorLocked(f.refs[ref], sha) {
		return fmt.Errorf("%s: non-fast-forward push to %s", f.Profile.Name, ref)
	}
	f.refs[ref] = sha
	f.allSHAs[sha] = true
	f.pushCount++
	f.record("push:" + ref + ":" + sha)
	return nil
}

// PushSnapshot writes a snapshot ref.
func (f *Forge) PushSnapshot(_ context.Context, platform, repo, ref, sha string, ts int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Caps.GitPush {
		return "", fmt.Errorf("%s: cannot write snapshots", f.Profile.Name)
	}
	tag := reconcile.SnapshotRef(ref, ts)
	if existing, ok := f.refs[tag]; ok && existing != sha {
		return "", fmt.Errorf("%s: snapshot %s already exists", f.Profile.Name, tag)
	}
	f.refs[tag] = sha
	f.allSHAs[sha] = true
	f.record("snapshot:" + ref)
	return tag, nil
}

// DeleteRef enforces the delete capability.
func (f *Forge) DeleteRef(_ context.Context, platform, repo, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Caps.GitRefDelete {
		return fmt.Errorf("%s: refuses ref deletion", f.Profile.Name)
	}
	delete(f.refs, ref)
	f.record("delete:" + ref)
	return nil
}

// MergeAttempt writes a deterministic merge ref.
func (f *Forge) MergeAttempt(_ context.Context, platform, repo, ref, base, ours, theirs string, ts int64) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Caps.GitPush {
		return "", "", fmt.Errorf("%s: cannot push merges", f.Profile.Name)
	}
	sha := "merge-" + ours + "-" + theirs
	mergeRef := reconcile.MergeRef(ref, ts)
	f.refs[mergeRef] = sha
	f.allSHAs[sha] = true
	f.record("merge:" + ref)
	return mergeRef, sha, nil
}

// Unprotect records the call.
func (f *Forge) Unprotect(context.Context, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Caps.GitProtected {
		return fmt.Errorf("%s: no protection model", f.Profile.Name)
	}
	f.record("unprotect")
	return nil
}

// Reprotect records the call.
func (f *Forge) Reprotect(context.Context, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Caps.GitProtected {
		return fmt.Errorf("%s: no protection model", f.Profile.Name)
	}
	f.record("reprotect")
	return nil
}

// Cluster multiplexes the executor's Remote across several fake platforms.
type Cluster struct {
	Forges map[string]*Forge
}

func (c Cluster) get(platform string) (*Forge, error) {
	f, ok := c.Forges[platform]
	if !ok {
		return nil, fmt.Errorf("no fake forge for platform %q", platform)
	}
	return f, nil
}

func (c Cluster) ReadRef(ctx context.Context, platform, repo, ref string) (string, error) {
	f, err := c.get(platform)
	if err != nil {
		return "", err
	}
	return f.ReadRef(ctx, platform, repo, ref)
}

func (c Cluster) RepoExists(ctx context.Context, platform, repo string) (bool, error) {
	f, err := c.get(platform)
	if err != nil {
		return false, err
	}
	return f.RepoExists(ctx, platform, repo)
}

func (c Cluster) CreateRepo(ctx context.Context, platform, repo, source string) error {
	f, err := c.get(platform)
	if err != nil {
		return err
	}
	return f.CreateRepo(ctx, platform, repo, source)
}

func (c Cluster) Push(ctx context.Context, platform, repo, ref, sha string, force bool, lease string) error {
	f, err := c.get(platform)
	if err != nil {
		return err
	}
	return f.Push(ctx, platform, repo, ref, sha, force, lease)
}

func (c Cluster) PushSnapshot(ctx context.Context, platform, repo, ref, sha string, ts int64) (string, error) {
	f, err := c.get(platform)
	if err != nil {
		return "", err
	}
	return f.PushSnapshot(ctx, platform, repo, ref, sha, ts)
}

func (c Cluster) DeleteRef(ctx context.Context, platform, repo, ref string) error {
	f, err := c.get(platform)
	if err != nil {
		return err
	}
	return f.DeleteRef(ctx, platform, repo, ref)
}

func (c Cluster) MergeAttempt(ctx context.Context, platform, repo, ref, base, ours, theirs string, ts int64) (string, string, error) {
	f, err := c.get(platform)
	if err != nil {
		return "", "", err
	}
	return f.MergeAttempt(ctx, platform, repo, ref, base, ours, theirs, ts)
}

func (c Cluster) Unprotect(ctx context.Context, platform, repo, ref string) error {
	f, err := c.get(platform)
	if err != nil {
		return err
	}
	return f.Unprotect(ctx, platform, repo, ref)
}

func (c Cluster) Reprotect(ctx context.Context, platform, repo, ref string) error {
	f, err := c.get(platform)
	if err != nil {
		return err
	}
	return f.Reprotect(ctx, platform, repo, ref)
}
