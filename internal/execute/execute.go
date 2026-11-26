// Package execute performs the actions the reconciler authorised. It journals
// before touching a remote, verifies every mutation by reading it back, writes a
// snapshot before any destructive step, and replays unfinished groups after a
// crash.
package execute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/state"
)

// Remote is the mutation surface. Git operations live here so tests can inject
// a hostile forge without starting a server.
type Remote interface {
	ReadRef(ctx context.Context, platform, repoSlug, ref string) (string, error)
	RepoExists(ctx context.Context, platform, repoSlug string) (bool, error)
	CreateRepo(ctx context.Context, platform, repoSlug, source string) error
	Push(ctx context.Context, platform, repoSlug, ref, sha string, force bool, leaseSHA string) error
	PushSnapshot(ctx context.Context, platform, repoSlug, ref, sha string, ts int64) (string, error)
	DeleteRef(ctx context.Context, platform, repoSlug, ref string) error
	MergeAttempt(ctx context.Context, platform, repoSlug, ref, base, ours, theirs string, ts int64) (mergeRef, sha string, err error)
	Unprotect(ctx context.Context, platform, repoSlug, ref string) error
	Reprotect(ctx context.Context, platform, repoSlug, ref string) error
}

// SafetyError means nuncio refused to act because an invariant would break. It
// maps to exit code 3.
type SafetyError struct {
	Reason string
}

func (e *SafetyError) Error() string { return "safety refusal: " + e.Reason }

// OperationalError is a remote or verification failure. Exit code 2.
type OperationalError struct {
	Op  string
	Err error
}

func (e *OperationalError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *OperationalError) Unwrap() error { return e.Err }

// Retryable marks an error that may succeed on a second attempt. Only transport
// errors should use it; a verification mismatch never does.
type Retryable struct{ Err error }

func (e *Retryable) Error() string { return e.Err.Error() }
func (e *Retryable) Unwrap() error { return e.Err }

// IsRetryable reports whether err opted into retries.
func IsRetryable(err error) bool {
	var r *Retryable
	return errors.As(err, &r)
}

// IsSafety reports whether err is a safety refusal.
func IsSafety(err error) bool {
	var s *SafetyError
	return errors.As(err, &s)
}

// Executor applies plans.
type Executor struct {
	Store       *state.Store
	Remote      Remote
	RepoID      int64
	Slug        string
	DryRun      bool
	MaxAttempts int
	Sleep       func(time.Duration)
	Now         func() time.Time
	Rand        *rand.Rand
	Log         func(format string, args ...any)
	seq         int
}

// New returns an executor with sane defaults.
func New(store *state.Store, remote Remote, repoID int64, slug string) *Executor {
	return &Executor{
		Store: store, Remote: remote, RepoID: repoID, Slug: slug,
		MaxAttempts: 3, Sleep: time.Sleep, Now: time.Now, Rand: rand.New(rand.NewSource(time.Now().UnixNano())),
		Log: func(string, ...any) {},
	}
}

// Outcome summarizes one action.
type Outcome struct {
	Action reconcile.Action
	Status string // done, skipped, planned, failed
	Err    error
}

// Result summarizes a run.
type Result struct {
	Outcomes []Outcome
	Executed int
	Skipped  int
	Planned  int
}

// Run executes every action in plan order, grouped per ref.
func (e *Executor) Run(ctx context.Context, plan reconcile.Result) (Result, error) {
	if !e.DryRun {
		if err := e.recoverPending(ctx); err != nil {
			return Result{}, err
		}
	}
	if err := validateSnapshots(plan.Actions); err != nil {
		return Result{}, err
	}
	var result Result
	for _, ref := range plan.Results {
		if len(ref.Actions) == 0 {
			continue
		}
		if err := e.runGroup(ctx, ref.Actions, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (e *Executor) runGroup(ctx context.Context, actions []reconcile.Action, result *Result) error {
	if e.DryRun {
		for _, a := range actions {
			result.Outcomes = append(result.Outcomes, Outcome{Action: a, Status: "planned"})
			result.Planned++
		}
		return nil
	}
	groupID := state.NewGroupID(e.Now(), e.seq)
	e.seq++
	journalActions := make([]state.JournalAction, len(actions))
	for i, a := range actions {
		payload, _ := json.Marshal(a)
		journalActions[i] = state.JournalAction{
			Action: string(a.Kind), Platform: a.Platform, Ref: a.Ref,
			FromSHA: a.FromSHA, ToSHA: a.ToSHA, Payload: string(payload),
		}
	}
	ids, err := e.Store.BeginGroup(ctx, e.RepoID, groupID, journalActions, e.now())
	if err != nil {
		return &OperationalError{Op: "journal group", Err: err}
	}
	// Snapshot actions that complete are remembered so a later force/delete in
	// this group can prove its precondition even if the process dies and the
	// group is replayed.
	snapshotDone := map[string]bool{}
	for i, a := range actions {
		out := e.executeOne(ctx, ids[i], a, true)
		result.Outcomes = append(result.Outcomes, out)
		switch out.Status {
		case "done":
			result.Executed++
		case "skipped":
			result.Skipped++
		}
		if out.Status == "done" || out.Status == "skipped" {
			if a.Kind == reconcile.ActionSnapshot {
				snapshotDone[a.Platform+"|"+a.Ref+"|"+a.FromSHA] = true
			}
		}
		if out.Err != nil {
			return out.Err
		}
	}
	return nil
}

func (e *Executor) executeOne(ctx context.Context, journalID int64, a reconcile.Action, enforceSnapshot bool) Outcome {
	if enforceSnapshot && a.Destructive() {
		if err := e.requireSnapshot(ctx, journalID, a); err != nil {
			_ = e.Store.MarkAction(ctx, journalID, "failed", err.Error(), 0, e.now())
			return Outcome{Action: a, Status: "failed", Err: err}
		}
	}
	attempts := 0
	var lastErr error
	for attempts < e.maxAttempts() {
		attempts++
		if !e.DryRun && journalID > 0 {
			if err := e.Store.MarkActionRunning(ctx, journalID, attempts, e.now()); err != nil {
				return Outcome{Action: a, Status: "failed", Err: &OperationalError{Op: "journal", Err: err}}
			}
		}
		status, err := e.perform(ctx, a)
		if err == nil {
			if !e.DryRun && journalID > 0 {
				_ = e.Store.MarkAction(ctx, journalID, status, "", attempts, e.now())
			}
			return Outcome{Action: a, Status: status}
		}
		lastErr = err
		if !IsRetryable(err) {
			break
		}
		if attempts < e.maxAttempts() {
			e.Sleep(e.backoff(attempts))
		}
	}
	if !e.DryRun && journalID > 0 {
		_ = e.Store.MarkAction(ctx, journalID, "failed", lastErr.Error(), attempts, e.now())
	}
	return Outcome{Action: a, Status: "failed", Err: &OperationalError{Op: string(a.Kind) + " " + a.Platform + "/" + a.Ref, Err: lastErr}}
}

func (e *Executor) perform(ctx context.Context, a reconcile.Action) (string, error) {
	r := e.Remote
	if r == nil {
		return "", errors.New("no remote configured")
	}
	switch a.Kind {
	case reconcile.ActionCreateRepo:
		exists, err := r.RepoExists(ctx, a.Platform, e.Slug)
		if err != nil {
			return "", err
		}
		if exists {
			return "skipped", nil
		}
		if err := r.CreateRepo(ctx, a.Platform, e.Slug, a.SourceSHA); err != nil {
			return "", err
		}
		exists, err = r.RepoExists(ctx, a.Platform, e.Slug)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", fmt.Errorf("repository creation was not observable afterwards")
		}
		return "done", nil
	case reconcile.ActionSnapshot:
		tag := a.SnapshotRef
		if tag == "" {
			tag = reconcile.SnapshotRef(a.Ref, e.Now().Unix())
		}
		current, err := r.ReadRef(ctx, a.Platform, e.Slug, tag)
		if err != nil {
			return "", err
		}
		if current == a.FromSHA {
			return "skipped", nil
		}
		if current != "" && current != a.FromSHA {
			return "", &SafetyError{Reason: fmt.Sprintf("snapshot ref %s already exists with %s; refusing to overwrite it", tag, short(current))}
		}
		written, err := r.PushSnapshot(ctx, a.Platform, e.Slug, a.Ref, a.FromSHA, e.Now().Unix())
		if err != nil {
			return "", err
		}
		if written == "" {
			written = tag
		}
		got, err := r.ReadRef(ctx, a.Platform, e.Slug, written)
		if err != nil {
			return "", err
		}
		if got != a.FromSHA {
			return "", fmt.Errorf("snapshot verification failed: %s has %s, want %s", written, short(got), short(a.FromSHA))
		}
		if e.Store != nil {
			_ = e.Store.RecordSnapshot(ctx, e.RepoID, a.Platform, a.Ref, a.FromSHA, written, e.now())
		}
		return "done", nil
	case reconcile.ActionPush, reconcile.ActionForcePush:
		force := a.Kind == reconcile.ActionForcePush
		current, err := r.ReadRef(ctx, a.Platform, e.Slug, a.Ref)
		if err != nil {
			return "", err
		}
		if current == a.ToSHA {
			return "skipped", nil
		}
		if force && a.LeaseSHA != "" && current != a.LeaseSHA {
			return "", &SafetyError{Reason: fmt.Sprintf("%s moved on %s since the plan (%s -> %s); re-plan before overwriting",
				a.Ref, a.Platform, short(a.LeaseSHA), short(current))}
		}
		if err := r.Push(ctx, a.Platform, e.Slug, a.Ref, a.ToSHA, force, a.LeaseSHA); err != nil {
			return "", err
		}
		got, err := r.ReadRef(ctx, a.Platform, e.Slug, a.Ref)
		if err != nil {
			return "", err
		}
		if got != a.ToSHA {
			return "", fmt.Errorf("push verification failed on %s: %s has %s, want %s", a.Platform, a.Ref, short(got), short(a.ToSHA))
		}
		return "done", nil
	case reconcile.ActionDeleteRef:
		current, err := r.ReadRef(ctx, a.Platform, e.Slug, a.Ref)
		if err != nil {
			return "", err
		}
		if current == "" {
			return "skipped", nil
		}
		if a.FromSHA != "" && current != a.FromSHA {
			return "", &SafetyError{Reason: fmt.Sprintf("%s moved on %s since the plan; refusing to delete", a.Ref, a.Platform)}
		}
		if err := r.DeleteRef(ctx, a.Platform, e.Slug, a.Ref); err != nil {
			return "", err
		}
		got, err := r.ReadRef(ctx, a.Platform, e.Slug, a.Ref)
		if err != nil {
			return "", err
		}
		if got != "" {
			return "", fmt.Errorf("delete verification failed on %s: %s still exists", a.Platform, a.Ref)
		}
		return "done", nil
	case reconcile.ActionMergeAttempt:
		tag := a.MergeRef
		if tag == "" {
			tag = reconcile.MergeRef(a.Ref, e.Now().Unix())
		}
		ref, sha, err := r.MergeAttempt(ctx, a.Platform, e.Slug, a.Ref, a.FromSHA, a.Ours, a.Theirs, e.Now().Unix())
		if err != nil {
			return "", err
		}
		got, err := r.ReadRef(ctx, a.Platform, e.Slug, ref)
		if err != nil {
			return "", err
		}
		if got != sha {
			return "", fmt.Errorf("merge attempt verification failed: %s has %s, want %s", ref, short(got), short(sha))
		}
		return "done", nil
	case reconcile.ActionUnprotect:
		if err := r.Unprotect(ctx, a.Platform, e.Slug, a.Ref); err != nil {
			return "", err
		}
		return "done", nil
	case reconcile.ActionReprotect:
		if err := r.Reprotect(ctx, a.Platform, e.Slug, a.Ref); err != nil {
			return "", err
		}
		return "done", nil
	case reconcile.ActionSetMeta:
		return "skipped", nil
	case reconcile.ActionReport:
		return "skipped", nil
	default:
		return "", fmt.Errorf("unknown action kind %q", a.Kind)
	}
}

// requireSnapshot checks the journal and the remote for proof that the tip about
// to be overwritten or deleted was preserved.
func (e *Executor) requireSnapshot(ctx context.Context, journalID int64, a reconcile.Action) error {
	// A snapshot must appear earlier in the same group (the planner guarantees
	// it; this is the runtime enforcement).
	if journalID > 0 {
		entries, err := e.Store.GroupActions(ctx, e.groupOf(ctx, journalID))
		if err == nil {
			for _, entry := range entries {
				if entry.ID >= journalID {
					break
				}
				if entry.Action == string(reconcile.ActionSnapshot) && entry.Platform == a.Platform &&
					entry.Ref == a.Ref && entry.FromSHA == a.FromSHA &&
					(entry.Status == "done" || entry.Status == "skipped") {
					return nil
				}
			}
		}
	}
	return &SafetyError{Reason: fmt.Sprintf("no completed snapshot for %s on %s; refusing %s", a.Ref, a.Platform, a.Kind)}
}

func (e *Executor) groupOf(ctx context.Context, journalID int64) string {
	group, err := e.Store.GroupOf(ctx, journalID)
	if err != nil {
		return ""
	}
	return group
}

func (e *Executor) recoverPending(ctx context.Context) error {
	entries, err := e.Store.PendingActions(ctx, e.RepoID)
	if err != nil {
		return &OperationalError{Op: "recover", Err: err}
	}
	if len(entries) == 0 {
		return nil
	}
	e.Log("recovering %d unfinished action(s) for %s", len(entries), e.Slug)
	groupIDs := []string{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !seen[entry.GroupID] {
			seen[entry.GroupID] = true
			groupIDs = append(groupIDs, entry.GroupID)
		}
	}
	for _, gid := range groupIDs {
		group, err := e.Store.GroupActions(ctx, gid)
		if err != nil {
			return &OperationalError{Op: "recover group", Err: err}
		}
		for _, entry := range group {
			if entry.Status == "done" || entry.Status == "skipped" {
				continue
			}
			var a reconcile.Action
			if err := json.Unmarshal([]byte(entry.Payload), &a); err != nil {
				return &OperationalError{Op: "recover payload", Err: err}
			}
			out := e.executeOne(ctx, entry.ID, a, true)
			if out.Err != nil {
				return out.Err
			}
		}
	}
	return nil
}

// validateSnapshots enforces the plan-level invariant.
func validateSnapshots(actions []reconcile.Action) error {
	snaps := map[string]map[string]bool{}
	for _, a := range actions {
		key := a.Platform + "|" + a.Ref
		if a.Destructive() {
			if !snaps[key][a.FromSHA] {
				return &SafetyError{Reason: fmt.Sprintf("plan has %s for %s on %s without a preceding snapshot", a.Kind, a.Ref, a.Platform)}
			}
		}
		if a.Kind == reconcile.ActionSnapshot {
			if snaps[key] == nil {
				snaps[key] = map[string]bool{}
			}
			snaps[key][a.FromSHA] = true
		}
	}
	return nil
}

func (e *Executor) maxAttempts() int {
	if e.MaxAttempts < 1 {
		return 1
	}
	return e.MaxAttempts
}

func (e *Executor) backoff(attempt int) time.Duration {
	base := 50 * time.Millisecond
	d := base << uint(attempt-1)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	if e.Rand != nil {
		d += time.Duration(e.Rand.Int63n(int64(d/2 + 1)))
	}
	return d
}

func (e *Executor) now() int64 {
	if e.Now == nil {
		return time.Now().Unix()
	}
	return e.Now().Unix()
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
