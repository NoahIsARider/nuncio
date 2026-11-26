package gitx

import (
	"context"
	"fmt"
	"strings"
)

// MergeTree writes a merge tree without touching any branch. It returns the
// tree object and whether the merge has conflicts; conflict markers are present
// in the tree, which is exactly what nuncio wants to preserve.
func (g *Git) MergeTree(ctx context.Context, base, ours, theirs string) (tree string, conflicts bool, err error) {
	var args []string
	if base != "" {
		args = []string{"merge-tree", "--write-tree", "--merge-base=" + base, ours, theirs}
	} else {
		args = []string{"merge-tree", "--write-tree", ours, theirs}
	}
	out, runErr := g.Run(ctx, nil, args...)
	if runErr != nil && ExitCode(runErr) != 1 {
		return "", false, runErr
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		if runErr != nil {
			return "", false, runErr
		}
		return "", false, fmt.Errorf("merge-tree produced no tree")
	}
	tree = strings.TrimSpace(lines[0])
	if !isObjectID(tree) {
		return "", false, fmt.Errorf("merge-tree produced an unexpected first line %q", tree)
	}
	return tree, runErr != nil && ExitCode(runErr) == 1, nil
}

// CommitTree creates a commit object for a tree and parents.
func (g *Git) CommitTree(ctx context.Context, tree string, parents []string, message, name, email string, ts int64) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		if p != "" {
			args = append(args, "-p", p)
		}
	}
	args = append(args, "-m", message)
	env := []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
		fmt.Sprintf("GIT_AUTHOR_DATE=@%d +0000", ts),
		fmt.Sprintf("GIT_COMMITTER_DATE=@%d +0000", ts),
	}
	out, err := g.Run(ctx, env, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// CreateMergeCommit builds a deterministic merge commit for a merge attempt.
// The same inputs and timestamp produce the same SHA, so replay is idempotent.
func (g *Git) CreateMergeCommit(ctx context.Context, base, ours, theirs, name, email string, ts int64) (sha string, conflicts bool, err error) {
	tree, conflicts, err := g.MergeTree(ctx, base, ours, theirs)
	if err != nil {
		return "", false, err
	}
	msg := fmt.Sprintf("nuncio: merge attempt %s..%s\n\nConflicts, if any, are preserved in this commit. Do not push this as history.\n", short(ours), short(theirs))
	sha, err = g.CommitTree(ctx, tree, []string{ours, theirs}, msg, name, email, ts)
	return sha, conflicts, err
}

// LooksLikeRewrite decides whether two unrelated tips share enough commit
// subjects to call one a rewrite of the other. It is a heuristic used only for
// the report's class column; it never triggers a destructive action.
func (g *Git) LooksLikeRewrite(ctx context.Context, a, b string, sample int) (bool, error) {
	if sample <= 0 {
		sample = 100
	}
	sa, err := g.CommitSubjects(ctx, a, sample)
	if err != nil {
		return false, err
	}
	sb, err := g.CommitSubjects(ctx, b, sample)
	if err != nil {
		return false, err
	}
	if len(sa) == 0 || len(sb) == 0 {
		return false, nil
	}
	set := map[string]bool{}
	for _, s := range sa {
		set[s] = true
	}
	shared := 0
	for _, s := range sb {
		if set[s] {
			shared++
		}
	}
	smaller := len(sa)
	if len(sb) < smaller {
		smaller = len(sb)
	}
	return shared >= 3 && shared*2 >= smaller, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
