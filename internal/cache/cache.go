// Package cache keeps one bare repository per logical repo. It is the local
// object database that makes ancestry tests cheap: ls-remote gives the observed
// tips, and only refs whose SHAs changed are fetched.
package cache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/gitx"
)

// Repo is one cached bare repository.
type Repo struct {
	Slug string
	Dir  string
	Git  *gitx.Git
}

// Cache is the root of all cached repositories.
type Cache struct {
	Root string
}

// New returns a cache rooted at dir.
func New(root string) *Cache { return &Cache{Root: root} }

// DirName maps a slug to a filesystem-safe, collision-resistant directory.
func DirName(slug string) string {
	var b strings.Builder
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	sum := sha1.Sum([]byte(slug))
	return b.String() + "-" + hex.EncodeToString(sum[:4]) + ".git"
}

// Open ensures and returns the bare repository for slug.
func (c *Cache) Open(ctx context.Context, slug string) (*Repo, error) {
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		return nil, err
	}
	dir := filepath.Join(c.Root, DirName(slug))
	if err := gitx.InitBare(ctx, dir); err != nil {
		return nil, err
	}
	return &Repo{Slug: slug, Dir: dir, Git: gitx.New(dir)}, nil
}

// Remove drops one cache directory. Tests use it; the daemon never deletes a
// repository as part of reconciliation.
func (c *Cache) Remove(slug string) error {
	return os.RemoveAll(filepath.Join(c.Root, DirName(slug)))
}

// ObservedRefName is the local namespace holding the last fetched tip.
func ObservedRefName(platform, ref string) string {
	return "refs/nuncio/observed/" + platform + "/" + strings.TrimPrefix(ref, "/")
}

// FetchRefs fetches the listed refs into per-platform observed namespaces.
func (r *Repo) FetchRefs(ctx context.Context, platform, remote string, env []string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	refspecs := make([]string, 0, len(refs))
	for _, ref := range refs {
		refspecs = append(refspecs, "+"+ref+":"+ObservedRefName(platform, ref))
	}
	return r.Git.FetchRefspecs(ctx, remote, env, refspecs...)
}

// ObservedRef reads the last fetched tip for a ref, or "".
func (r *Repo) ObservedRef(ctx context.Context, platform, ref string) string {
	sha, err := r.Git.RevParse(ctx, ObservedRefName(platform, ref))
	if err != nil {
		return ""
	}
	return sha
}

// EnsureObjects fetches any missing SHAs from the given remote.
func (r *Repo) EnsureObjects(ctx context.Context, remote string, env []string, shas []string) error {
	for _, sha := range shas {
		if sha == "" || r.Git.HasObject(ctx, sha) {
			continue
		}
		if err := r.Git.FetchSHA(ctx, remote, env, sha); err != nil {
			return fmt.Errorf("fetch %s from %s: %w", short(sha), remote, err)
		}
	}
	return nil
}

// HasAll reports whether every SHA exists locally.
func (r *Repo) HasAll(ctx context.Context, shas []string) bool {
	for _, sha := range shas {
		if sha == "" {
			continue
		}
		if !r.Git.HasObject(ctx, sha) {
			return false
		}
	}
	return true
}

// Ancestry computes directed ancestry facts for all pairs in shas.
type Fact struct {
	A, B     string
	Ancestor bool
	Known    bool
}

// Ancestry returns a fact for every ordered pair once.
func (r *Repo) Ancestry(ctx context.Context, shas []string) []Fact {
	var out []Fact
	for i := 0; i < len(shas); i++ {
		for j := i + 1; j < len(shas); j++ {
			if shas[i] == shas[j] {
				continue
			}
			anc, known := r.Git.IsAncestor(ctx, shas[i], shas[j])
			out = append(out, Fact{A: shas[i], B: shas[j], Ancestor: anc, Known: known})
			if !anc && known {
				anc2, known2 := r.Git.IsAncestor(ctx, shas[j], shas[i])
				out = append(out, Fact{A: shas[j], B: shas[i], Ancestor: anc2, Known: known2})
			}
		}
	}
	return out
}

// MergeBase exposes the git merge base.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	return r.Git.MergeBase(ctx, a, b)
}

// LooksLikeRewrite exposes the subject-overlap heuristic.
func (r *Repo) LooksLikeRewrite(ctx context.Context, a, b string) (bool, error) {
	return r.Git.LooksLikeRewrite(ctx, a, b, 100)
}

// CreateMergeCommit builds a deterministic merge commit locally.
func (r *Repo) CreateMergeCommit(ctx context.Context, base, ours, theirs, name, email string, ts int64) (string, bool, error) {
	return r.Git.CreateMergeCommit(ctx, base, ours, theirs, name, email, ts)
}

// LostResult is the proof a human asked for.
type LostResult struct {
	Count    int      `json:"count"`
	SHAs     []string `json:"shas,omitempty"`
	Complete bool     `json:"complete"`
	Missing  []string `json:"missing,omitempty"`
}

// ErrMissingObjects means the answer cannot be computed truthfully because some
// observed objects are not local. Nuncio must not report 0 in that case.
type ErrMissingObjects struct {
	SHAs []string
}

func (e *ErrMissingObjects) Error() string {
	return fmt.Sprintf("lost-commit count unavailable: %d object(s) missing locally", len(e.SHAs))
}

// Lost counts commits reachable from universe that are reachable from no alive
// ref (current refs plus snapshots). It refuses to answer when objects are
// missing.
func (r *Repo) Lost(ctx context.Context, universe, alive []string) (LostResult, error) {
	missing := make([]string, 0)
	for _, sha := range universe {
		if sha == "" {
			continue
		}
		if !r.Git.HasObject(ctx, sha) {
			missing = append(missing, sha)
		}
	}
	for _, sha := range alive {
		if sha == "" {
			continue
		}
		if !r.Git.HasObject(ctx, sha) {
			missing = append(missing, sha)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return LostResult{Complete: false, Missing: dedupe(missing)}, &ErrMissingObjects{SHAs: dedupe(missing)}
	}
	universeSet, err := r.Git.RevList(ctx, universe)
	if err != nil {
		return LostResult{}, err
	}
	aliveSet, err := r.Git.RevList(ctx, alive)
	if err != nil {
		return LostResult{}, err
	}
	lost := make([]string, 0)
	for sha := range universeSet {
		if _, ok := aliveSet[sha]; !ok {
			lost = append(lost, sha)
		}
	}
	sort.Strings(lost)
	return LostResult{Count: len(lost), SHAs: lost, Complete: true}, nil
}

// Reachable returns the set of commits reachable from shas.
func (r *Repo) Reachable(ctx context.Context, shas []string) (map[string]struct{}, error) {
	return r.Git.RevList(ctx, shas)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
