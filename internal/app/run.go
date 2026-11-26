package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/NoahIsARider/nuncio/internal/cache"
	"github.com/NoahIsARider/nuncio/internal/execute"
	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/report"
	"github.com/NoahIsARider/nuncio/internal/state"
)

func timeFromUnix(ts int64) time.Time { return time.Unix(ts, 0) }

func outcomeData(in []execute.Outcome) []report.Outcome {
	out := make([]report.Outcome, 0, len(in))
	for _, o := range in {
		errText := ""
		if o.Err != nil {
			errText = o.Err.Error()
		}
		out = append(out, report.Outcome{
			Action: string(o.Action.Kind), Platform: o.Action.Platform,
			Ref: o.Action.Ref, Status: o.Status, Error: errText,
		})
	}
	return out
}

// Run is one completed (or simulated) reconciliation.
type Run struct {
	Repo      string
	DryRun    bool
	Plan      reconcile.Result
	Outcomes  []execute.Outcome
	Snapshots []state.Snapshot
	Lost      report.Lost
	Platforms []report.PlatformInfo
	Unknown   map[string]string
	Notices   []string

	Started  int64
	Finished int64
}

// Data converts a run into the report's data model.
func (r *Run) Data() report.Data {
	data := report.Data{
		Repo: r.Repo, Platforms: r.Platforms, DryRun: r.DryRun,
		Results: r.Plan.Results, Actions: r.Plan.Actions,
		Outcomes: outcomeData(r.Outcomes), Snapshots: r.Snapshots, Lost: r.Lost,
	}
	if r.Finished != 0 {
		data.GeneratedAt = timeFromUnix(r.Finished)
	}
	return data
}

// Findings counts what deserves a human look.
func (r *Run) Findings() int { return r.Data().Findings() }

// Operational reports whether any platform failed to answer. It is what makes a
// run exit 2 rather than 0 or 1.
func (r *Run) Operational() bool { return len(r.Unknown) > 0 }

// Plan observes and plans without mutating anything.
func (a *App) Plan(ctx context.Context, slug string) (*Run, error) {
	return a.run(ctx, slug, true, false)
}

// Sync observes, plans, executes (unless dryRun) and verifies the result.
func (a *App) Sync(ctx context.Context, slug string, dryRun bool) (*Run, error) {
	return a.run(ctx, slug, dryRun, true)
}

func (a *App) run(ctx context.Context, slug string, dryRun, executeActions bool) (*Run, error) {
	specs, notices, err := a.RepoSpecs(ctx)
	if err != nil {
		return nil, err
	}
	var spec *RepoSpec
	for i := range specs {
		if specs[i].Slug == slug {
			spec = &specs[i]
			break
		}
	}
	if spec == nil {
		return nil, fmt.Errorf("repo %q is not managed by this config", slug)
	}

	prep, err := a.prepare(ctx, *spec)
	if err != nil {
		return nil, err
	}
	run := &Run{
		Repo: slug, DryRun: dryRun, Plan: prep.Plan, Platforms: prep.Info,
		Unknown: prep.Obs.Unknown, Notices: notices, Started: a.Now().Unix(),
	}

	if executeActions {
		executor := execute.New(a.Store, prep.Remote, prep.RepoID, slug)
		executor.DryRun = dryRun
		executor.Now = a.Now
		executor.Log = a.Logf
		result, execErr := executor.Run(ctx, prep.Plan)
		run.Outcomes = result.Outcomes
		if execErr != nil {
			run.Finished = a.Now().Unix()
			run.Snapshots = a.snapshots(ctx, prep.RepoID)
			return run, execErr
		}
		if !dryRun {
			// Re-read the forges so the report describes the world after the
			// pushes, not the plan that produced them.
			if fresh, err := a.observe(ctx, *spec, prep.CacheRepo); err == nil {
				prep.Obs = fresh
				run.Unknown = fresh.Unknown
				a.refreshResults(ctx, *spec, prep)
			} else {
				a.Logf("post-sync observation for %s: %v", slug, err)
			}
		}
	}

	run.Lost = a.computeLost(ctx, prep.CacheRepo, prep.Obs, prep.RepoID)
	run.Snapshots = a.snapshots(ctx, prep.RepoID)
	run.Finished = a.Now().Unix()

	if executeActions {
		a.persistReport(ctx, prep.RepoID, run)
	}
	return run, nil
}

func (a *App) snapshots(ctx context.Context, repoID int64) []state.Snapshot {
	snaps, err := a.Store.ListSnapshots(ctx, repoID)
	if err != nil {
		a.Logf("list snapshots: %v", err)
		return nil
	}
	return snaps
}

func (a *App) computeLost(ctx context.Context, cacheRepo *cache.Repo, obs Observation, repoID int64) report.Lost {
	universe, err := a.Store.UniverseSHAs(ctx, repoID)
	if err != nil {
		return report.Lost{Complete: false}
	}
	alive := aliveSHAs(obs)
	if snapshotSHAs, err := a.Store.SnapshotSHAs(ctx, repoID); err == nil {
		alive = append(alive, snapshotSHAs...)
	}
	alive = dedupe(alive)
	result, err := cacheRepo.Lost(ctx, universe, alive)
	if err != nil {
		var missing *cache.ErrMissingObjects
		if errors.As(err, &missing) {
			return report.Lost{Complete: false, Missing: missing.SHAs}
		}
		a.Logf("lost computation: %v", err)
		return report.Lost{Complete: false}
	}
	return report.Lost{Count: result.Count, Complete: result.Complete, Missing: result.Missing}
}

// aliveSHAs is every tip currently advertised by a platform, including snapshot
// and merge refs. Those refs are exactly what keeps a commit reachable.
func aliveSHAs(obs Observation) []string {
	var out []string
	for _, refs := range obs.AllRefs {
		for _, rr := range refs {
			if rr.SHA != "" {
				out = append(out, rr.SHA)
			}
		}
	}
	return out
}

// Lost computes the lost-commit proof on its own.
func (a *App) Lost(ctx context.Context, slug string) (report.Lost, []report.PlatformInfo, map[string]string, error) {
	spec, err := a.Spec(ctx, slug)
	if err != nil {
		return report.Lost{}, nil, nil, err
	}
	prep, err := a.prepare(ctx, spec)
	if err != nil {
		return report.Lost{}, nil, nil, err
	}
	return a.computeLost(ctx, prep.CacheRepo, prep.Obs, prep.RepoID), prep.Info, prep.Obs.Unknown, nil
}

func (a *App) persistReport(ctx context.Context, repoID int64, run *Run) {
	data := run.Data()
	var buf bytes.Buffer
	if err := data.RenderJSON(&buf); err != nil {
		a.Logf("render report for %s: %v", run.Repo, err)
		return
	}
	lost := int64(run.Lost.Count)
	if !run.Lost.Complete {
		lost = -1 // "unknown" must not be stored as a false zero
	}
	if _, err := a.Store.RecordReport(ctx, state.ReportRecord{
		RepoID: repoID, StartedAt: run.Started, FinishedAt: run.Finished,
		DryRun: run.DryRun, Lost: lost, JSON: buf.String(),
	}); err != nil {
		a.Logf("record report for %s: %v", run.Repo, err)
	}
}

// ReportsSince returns stored report runs for a repo within the duration.
func (a *App) ReportsSince(ctx context.Context, slug string, sinceUnix int64) ([]state.ReportRecord, error) {
	spec, err := a.Spec(ctx, slug)
	if err != nil {
		return nil, err
	}
	row, err := a.Store.GetRepo(ctx, spec.Slug)
	if err != nil {
		return nil, nil
	}
	return a.Store.ReportsSince(ctx, row.ID, sinceUnix)
}

// RepoSummaries returns one line per managed repo for the status page. It does
// not contact any forge.
func (a *App) RepoSummaries(ctx context.Context) ([]RepoSpec, error) {
	specs, _, err := a.RepoSpecs(ctx)
	return specs, err
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// refreshResults replaces each ref's class with the post-execution truth, while
// keeping the executed actions and their notices. A successful fast-forward
// therefore stops being a finding once it has actually happened, which is what
// exit code 1 is supposed to mean: something is still waiting for a human.
func (a *App) refreshResults(ctx context.Context, spec RepoSpec, prep *Prepared) {
	relations, bases, rewrites := a.computeRelations(ctx, prep.CacheRepo, prep.Obs)
	policy, err := a.policyFor(spec)
	if err != nil {
		return
	}
	var platforms []reconcile.Platform
	for _, name := range policy.Platforms {
		if _, ok := a.Adapters[name]; !ok {
			continue
		}
		platforms = append(platforms, reconcile.Platform{Name: name, Caps: a.platformCaps(name)})
	}
	post := reconcile.Plan(reconcile.Input{
		Repo: spec.Slug, Policy: policy, Platforms: platforms,
		Refs: prep.Obs.Refs, Prior: prep.Prior, RepoExists: prep.Obs.RepoExists,
		Unknown: prep.Obs.Unknown, Relations: relations, Rewrites: rewrites,
		Bases: bases, Now: a.Now().Unix(),
	})
	byRef := make(map[string]reconcile.RefResult, len(post.Results))
	for _, res := range post.Results {
		byRef[res.Ref] = res
	}
	for i := range prep.Plan.Results {
		if fresh, ok := byRef[prep.Plan.Results[i].Ref]; ok {
			prep.Plan.Results[i].Class = fresh.Class
			prep.Plan.Results[i].Peers = fresh.Peers
			prep.Plan.Results[i].Base = fresh.Base
			prep.Plan.Results[i].Blocked = fresh.Blocked
		}
	}
}
