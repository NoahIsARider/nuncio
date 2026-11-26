// Package gitx wraps the git command line. It is the only place in nuncio that
// executes git, and it refuses MSYS-style path arguments outright.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Git is a git invocation bound to a repository directory and a base
// environment.
type Git struct {
	Bin string
	Dir string
	Env []string
}

// New returns a Git bound to dir.
func New(dir string) *Git {
	bin := os.Getenv("GIT")
	if bin == "" {
		bin = "git"
	}
	return &Git{Bin: bin, Dir: dir}
}

// WithEnv returns a copy with extra environment entries.
func (g *Git) WithEnv(env ...string) *Git {
	cp := *g
	cp.Env = append(append([]string{}, g.Env...), env...)
	return &cp
}

// Error carries the failed command without secrets. Environment is never
// printed because it can hold credentials.
type Error struct {
	Dir    string
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s (in %s): %s", strings.Join(e.Args, " "), e.Dir, msg)
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns git's exit code, or -1.
func ExitCode(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		var ee *exec.ExitError
		if errors.As(ge.Err, &ee) {
			return ee.ExitCode()
		}
	}
	return -1
}

func (g *Git) command(ctx context.Context, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Dir = g.Dir
	cmd.Env = append(os.Environ(), g.Env...)
	cmd.Env = append(cmd.Env, env...)
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

// Run executes git and returns stdout.
func (g *Git) Run(ctx context.Context, env []string, args ...string) (string, error) {
	if err := AssertNoMSYSArgs(args); err != nil {
		return "", err
	}
	cmd := g.command(ctx, env, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &Error{Dir: g.Dir, Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// RunInput executes git with stdin.
func (g *Git) RunInput(ctx context.Context, env []string, input string, args ...string) (string, error) {
	if err := AssertNoMSYSArgs(args); err != nil {
		return "", err
	}
	cmd := g.command(ctx, env, args...)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &Error{Dir: g.Dir, Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// InitBare creates a bare repository at dir if it does not exist.
func InitBare(ctx context.Context, dir string) error {
	native, err := RequireNative(dir)
	if err != nil {
		return err
	}
	g := New(filepath.Dir(native))
	if _, err := os.Stat(native); err == nil {
		if _, err := os.Stat(filepath.Join(native, "HEAD")); err == nil {
			return nil
		}
	}
	if _, err := g.Run(ctx, nil, "init", "--bare", "--quiet", native); err != nil {
		return err
	}
	return nil
}

var localPathLike = regexp.MustCompile(`^(?:[A-Za-z]:[\\/]|/|\.\.?[\\/])`)

// NormalizeRemote turns a local path into a file:// URL. URLs are left alone.
// This keeps `git push D:/x.git` on Windows out of MSYS interpretation.
func NormalizeRemote(remote string) string {
	if remote == "" {
		return remote
	}
	if strings.Contains(remote, "://") || strings.Contains(remote, "@") {
		return remote
	}
	if localPathLike.MatchString(remote) {
		return FileURL(remote)
	}
	return remote
}

// RemoteRef is one line of ls-remote.
type RemoteRef struct {
	SHA    string
	Peeled string // for annotated tags, the peeled commit
}

// LsRemote lists refs. Patterns are optional.
func (g *Git) LsRemote(ctx context.Context, remote string, env []string, patterns ...string) (map[string]RemoteRef, error) {
	args := []string{"ls-remote", NormalizeRemote(remote)}
	args = append(args, patterns...)
	out, err := g.Run(ctx, env, args...)
	if err != nil {
		return nil, err
	}
	refs := map[string]RemoteRef{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sha, ref := fields[0], fields[1]
		if strings.HasSuffix(ref, "^{}") {
			base := strings.TrimSuffix(ref, "^{}")
			r := refs[base]
			r.Peeled = sha
			refs[base] = r
			continue
		}
		r := refs[ref]
		r.SHA = sha
		refs[ref] = r
	}
	return refs, nil
}

// FetchRefspecs fetches exact refspecs into the local repository. It never
// prunes: objects that disappear from a remote are preserved locally.
func (g *Git) FetchRefspecs(ctx context.Context, remote string, env []string, refspecs ...string) error {
	if len(refspecs) == 0 {
		return nil
	}
	args := []string{"fetch", "--no-tags", "--force", "--quiet", NormalizeRemote(remote)}
	args = append(args, refspecs...)
	_, err := g.Run(ctx, env, args...)
	return err
}

// FetchSHA makes one object reachable locally without moving any user ref.
func (g *Git) FetchSHA(ctx context.Context, remote string, env []string, sha string) error {
	if sha == "" {
		return nil
	}
	dst := "refs/nuncio/fetched/" + sha
	_, err := g.Run(ctx, env, "fetch", "--no-tags", "--force", "--quiet", NormalizeRemote(remote), "+"+sha+":"+dst)
	return err
}

// HasObject reports whether sha exists in the object database.
func (g *Git) HasObject(ctx context.Context, sha string) bool {
	_, err := g.Run(ctx, nil, "cat-file", "-e", sha)
	return err == nil
}

// IsAncestor reports whether a is an ancestor of b.
func (g *Git) IsAncestor(ctx context.Context, a, b string) (bool, bool) {
	_, err := g.Run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, true
	}
	if ExitCode(err) == 1 {
		return false, true
	}
	return false, false
}

// MergeBase returns the best common ancestor, or "" when unrelated.
func (g *Git) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := g.Run(ctx, nil, "merge-base", a, b)
	if err != nil {
		if ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// RevParse resolves a revision.
func (g *Git) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := g.Run(ctx, nil, "rev-parse", "--verify", rev)
	return strings.TrimSpace(out), err
}

// PeelCommit resolves a tag object or commit to a commit SHA.
func (g *Git) PeelCommit(ctx context.Context, rev string) (string, bool) {
	out, err := g.Run(ctx, nil, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// RevList returns every commit reachable from shas.
func (g *Git) RevList(ctx context.Context, shas []string) (map[string]struct{}, error) {
	clean := make([]string, 0, len(shas))
	for _, s := range shas {
		if s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return map[string]struct{}{}, nil
	}
	out, err := g.RunInput(ctx, nil, strings.Join(clean, "\n")+"\n", "rev-list", "--stdin")
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			set[line] = struct{}{}
		}
	}
	return set, nil
}

// CommitSubjects returns up to limit subjects reachable from rev.
func (g *Git) CommitSubjects(ctx context.Context, rev string, limit int) ([]string, error) {
	out, err := g.Run(ctx, nil, "log", fmt.Sprintf("-%d", limit), "--format=%s", rev)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			subjects = append(subjects, line)
		}
	}
	return subjects, nil
}

// PushOptions controls one push.
type PushOptions struct {
	Force          bool
	ForceWithLease string // "<ref>:<expected sha>"
	PushOptions    []string
	DryRun         bool
}

// Push runs a single explicit refspec push. Mirror mode is never generated.
func (g *Git) Push(ctx context.Context, remote string, env []string, refspec string, opts PushOptions) error {
	args := []string{"push", "--porcelain"}
	if opts.DryRun {
		args = append(args, "--dry-run")
	}
	if opts.ForceWithLease != "" {
		args = append(args, "--force-with-lease="+opts.ForceWithLease)
	} else if opts.Force {
		args = append(args, "--force")
	}
	for _, o := range opts.PushOptions {
		args = append(args, "-o", o)
	}
	args = append(args, NormalizeRemote(remote), refspec)
	_, err := g.Run(ctx, env, args...)
	return err
}

// FetchLFS fetches all LFS objects known to the remote.
func (g *Git) FetchLFS(ctx context.Context, remote string, env []string) error {
	_, err := g.Run(ctx, env, "lfs", "fetch", "--all", NormalizeRemote(remote))
	return err
}

// PushLFS pushes all LFS objects to the remote.
func (g *Git) PushLFS(ctx context.Context, remote string, env []string) error {
	_, err := g.Run(ctx, env, "lfs", "push", "--all", NormalizeRemote(remote))
	return err
}

// HasLFS reports whether git-lfs is installed.
func (g *Git) HasLFS(ctx context.Context) bool {
	_, err := g.Run(ctx, nil, "lfs", "version")
	return err == nil
}

// UsesLFS inspects .gitattributes at rev for a filter=lfs entry.
func (g *Git) UsesLFS(ctx context.Context, rev string) (bool, error) {
	out, err := g.Run(ctx, nil, "cat-file", "-p", rev+":.gitattributes")
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "Not a valid object name") {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(out, "filter=lfs"), nil
}
