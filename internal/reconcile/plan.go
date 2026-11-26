package reconcile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/config"
)

// Plan classifies every ref and returns the actions the policy authorises.
func Plan(in Input) Result {
	out := Result{Repo: in.Repo}
	pl := &planner{in: in, created: map[string]bool{}}
	for _, ref := range PlanRefs(in) {
		res := pl.ref(ref)
		if res == nil {
			continue
		}
		out.Results = append(out.Results, *res)
		out.Actions = append(out.Actions, res.Actions...)
	}
	return out
}

type planner struct {
	in      Input
	created map[string]bool
}

func (pl *planner) ref(ref string) *RefResult {
	in := pl.in
	pol := in.Policy.ForRef(ref)
	res := &RefResult{Ref: ref, Class: ClassEqual, Authority: pol.Authority}
	byName := map[string]Platform{}
	for _, p := range in.Platforms {
		byName[p.Name] = p
	}
	var known []Platform
	var unknown []string
	for _, name := range in.Policy.Platforms {
		p, ok := byName[name]
		if !ok {
			res.Notices = append(res.Notices, Notice{Platform: name, Kind: "unreachable", Text: "platform is referenced by policy but not configured"})
			unknown = append(unknown, name)
			continue
		}
		peer := Peer{Platform: name}
		if errText, down := in.Unknown[name]; down {
			peer.Unknown = true
			res.Notices = append(res.Notices, Notice{Platform: name, Kind: "unreachable", Text: errText})
			unknown = append(unknown, name)
		} else {
			if obs, ok := in.Refs[ref][name]; ok && obs.Present {
				peer.Present = true
				peer.SHA = obs.SHA
			}
			known = append(known, p)
		}
		res.Peers = append(res.Peers, peer)
	}
	sort.Slice(res.Peers, func(i, j int) bool { return res.Peers[i].Platform < res.Peers[j].Platform })
	if len(known) == 0 {
		res.Class = ClassUnreachable
		return res
	}

	present := map[string]Observation{}
	for _, p := range known {
		if obs, ok := in.Refs[ref][p.Name]; ok && obs.Present {
			present[p.Name] = obs
		}
	}
	prior := in.Prior[ref]
	priorPresent := map[string]Prior{}
	for name, pr := range prior {
		if pr.Present {
			priorPresent[name] = pr
		}
	}

	// Nothing present anywhere we can see.
	if len(present) == 0 {
		if len(priorPresent) == 0 {
			if len(unknown) > 0 {
				res.Class = ClassUnreachable
				return res
			}
			return nil // never existed: no row, no noise
		}
		res.Class = ClassDeleted
		pl.handleDelete(res, pol, known, priorPresent, map[string]Observation{}, unknown)
		return res
	}

	// Per-platform deletion: present on some, previously present and now absent
	// on others.
	deletedPlatforms := map[string]Prior{}
	for _, p := range known {
		if _, ok := present[p.Name]; ok {
			continue
		}
		if pr, ok := priorPresent[p.Name]; ok {
			deletedPlatforms[p.Name] = pr
		}
	}
	if len(deletedPlatforms) > 0 {
		res.Class = ClassDeleted
		for _, p := range sortedPlatformNames(deletedPlatforms) {
			res.Notices = append(res.Notices, Notice{
				Platform: p, Kind: "deleted",
				Text: fmt.Sprintf("deleted on %s; last seen %s", p, shortSHA(deletedPlatforms[p].SHA)),
			})
		}
		if pol.OnDelete == config.DeletePropagate {
			pl.handleDelete(res, pol, known, deletedPlatforms, present, unknown)
		} else {
			res.Notices = append(res.Notices, Notice{Kind: "info", Text: "on_delete = ignore, so the deletion is not propagated and the ref is not resurrected"})
		}
		return res
	}

	distinct := distinctSHAs(present)
	priorChanged := priorChanged(priorPresent, present)

	if len(distinct) == 1 {
		if priorChanged && isTag(ref) {
			res.Class = ClassTagMoved
			res.Notices = append(res.Notices, Notice{Kind: "tag", Text: "tag object moved; snapshot policy applies"})
		}
		var missing []Platform
		for _, p := range known {
			if _, ok := present[p.Name]; !ok {
				missing = append(missing, p)
			}
		}
		if len(missing) == 0 {
			return res
		}
		if res.Class != ClassTagMoved {
			res.Class = ClassCreated
		}
		sha := distinct[0]
		emitted := false
		for _, p := range missing {
			if pl.push(res, p, ref, sha, "ref exists on a peer, absent here") {
				emitted = true
			}
		}
		if !emitted {
			pl.adjustBlocked(res)
		}
		return res
	}

	// Two or more distinct tips.
	if isTag(ref) && priorChanged {
		res.Class = ClassTagMoved
		pl.diverged(res, pol, ref, known, present, distinct, unknown)
		return res
	}
	if tip, ok := fastForwardTip(distinct, in.Relations); ok {
		res.Class = ClassFastForward
		emitted := false
		for _, p := range known {
			obs, on := present[p.Name]
			if !on {
				if pl.push(res, p, ref, tip, "ref missing here") {
					emitted = true
				}
				continue
			}
			if obs.SHA == tip {
				continue
			}
			if ancestor, knownRel := in.Relations.Ancestor(obs.SHA, tip); !knownRel || !ancestor {
				res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "capability", Text: "ancestry unknown; refusing to advance this platform"})
				continue
			}
			if pl.push(res, p, ref, tip, "fast-forward from "+shortSHA(obs.SHA)) {
				emitted = true
			}
		}
		if !emitted && len(res.Notices) > 0 {
			pl.adjustBlocked(res)
		}
		return res
	}

	if in.Rewrites[ref] && len(distinct) == 2 {
		res.Class = ClassHistoryRewrite
	} else {
		res.Class = ClassDivergence
	}
	pl.diverged(res, pol, ref, known, present, distinct, unknown)
	return res
}

func (pl *planner) diverged(res *RefResult, pol config.RefPolicy, ref string, known []Platform, present map[string]Observation, distinct []string, unknown []string) {
	in := pl.in
	base := in.Bases[ref]
	res.Base = base
	res.Notices = append(res.Notices, Notice{Kind: "info", Text: fmt.Sprintf("%d distinct tips: %s", len(distinct), shortList(distinct))})

	switch pol.OnDivergence {
	case config.DivergenceMerge:
		if len(distinct) != 2 {
			pl.park(res, known, present, "n-way divergence is never resolved automatically")
			return
		}
		if base == "" {
			pl.park(res, known, present, "merge base unavailable, so the divergence is parked")
			return
		}
		pl.snapshotAll(res, known, present, "merge attempt")
		ours, theirs := mergeSides(pol.Authority, present, distinct)
		for _, p := range known {
			if _, ok := present[p.Name]; !ok {
				continue
			}
			if glob, reserved := p.Caps.Reserved(ref); reserved {
				res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "namespace", Text: "reserved namespace " + glob})
				continue
			}
			if !p.Caps.GitPush {
				res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "capability", Text: "cannot push merge attempt"})
				continue
			}
			res.Actions = append(res.Actions, Action{
				Kind: ActionMergeAttempt, Platform: p.Name, Ref: ref,
				FromSHA: base, Ours: ours, Theirs: theirs,
				MergeRef: MergeRef(ref, in.Now),
				Reason:   "conflicts preserved for a human",
			})
		}
	case config.DivergenceForceFromAuthority:
		if len(distinct) > 2 {
			pl.park(res, known, present, "n-way divergence is never resolved automatically")
			return
		}
		if pol.Authority == "" || pol.Authority == config.AuthorityNone {
			pl.park(res, known, present, "force-from-authority was requested but no authority is named")
			return
		}
		authObs, ok := present[pol.Authority]
		if !ok {
			pl.park(res, known, present, "authority "+pol.Authority+" has no tip for this ref")
			return
		}
		forceEmitted := false
		for _, p := range known {
			obs, on := present[p.Name]
			if !on {
				pl.push(res, p, ref, authObs.SHA, "created from authority "+pol.Authority)
				continue
			}
			if obs.SHA == authObs.SHA {
				continue
			}
			if pl.snapshot(res, p, ref, obs.SHA, "before force-push from authority "+pol.Authority) {
				if pl.forcePush(res, p, ref, authObs.SHA, obs.SHA) {
					forceEmitted = true
				}
			}
		}
		if !forceEmitted && res.Blocked != "" {
			pl.adjustBlocked(res)
		}
	default: // park
		pl.park(res, known, present, "authority is intent, not a licence to overwrite")
	}
}

func (pl *planner) park(res *RefResult, known []Platform, present map[string]Observation, why string) {
	pl.snapshotAll(res, known, present, "divergence parked")
	res.Notices = append(res.Notices, Notice{Kind: "park", Text: why})
}

func (pl *planner) snapshotAll(res *RefResult, known []Platform, present map[string]Observation, reason string) {
	emitted := false
	for _, p := range known {
		obs, ok := present[p.Name]
		if !ok {
			continue
		}
		if pl.snapshot(res, p, res.Ref, obs.SHA, reason) {
			emitted = true
		}
	}
	if !emitted && len(present) > 0 {
		res.Blocked = "capability:snapshot"
	}
}

func (pl *planner) handleDelete(res *RefResult, pol config.RefPolicy, known []Platform, deleted map[string]Prior, present map[string]Observation, unknown []string) {
	if pol.OnDelete != config.DeletePropagate {
		return
	}
	// Snapshot every tip we are about to lose, including tips already gone on
	// one platform, before any delete is emitted.
	canDelete := true
	for _, p := range known {
		if obs, ok := present[p.Name]; ok {
			if !pl.snapshot(res, p, res.Ref, obs.SHA, "before propagating deletion") {
				canDelete = false
			}
		} else if pr, ok := deleted[p.Name]; ok {
			if !pl.snapshot(res, p, res.Ref, pr.SHA, "preserving tip deleted on this platform") {
				canDelete = false
			}
		}
	}
	if !canDelete {
		res.Notices = append(res.Notices, Notice{Kind: "capability", Text: "deletion not propagated because a snapshot could not be written"})
		res.Blocked = "capability:snapshot"
		return
	}
	for _, p := range known {
		if _, ok := present[p.Name]; !ok {
			continue // already absent
		}
		if !p.Caps.GitPush || !p.Caps.GitRefDelete {
			res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "capability", Text: "platform cannot delete refs; deletion not propagated here"})
			res.Blocked = "capability:delete"
			continue
		}
		res.Actions = append(res.Actions, Action{
			Kind: ActionDeleteRef, Platform: p.Name, Ref: res.Ref,
			FromSHA: present[p.Name].SHA, Reason: "on_delete = propagate",
		})
	}
}

func (pl *planner) push(res *RefResult, p Platform, ref, sha, reason string) bool {
	if glob, reserved := p.Caps.Reserved(ref); reserved {
		res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "namespace", Text: "reserved namespace " + glob + ", skipped by design"})
		res.Blocked = "namespace:" + glob
		return false
	}
	if !p.Caps.GitPush {
		res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "capability", Text: "platform cannot accept pushes"})
		res.Blocked = "capability:push"
		return false
	}
	if !pl.ensureRepo(res, p) {
		return false
	}
	res.Actions = append(res.Actions, Action{
		Kind: ActionPush, Platform: p.Name, Ref: ref, ToSHA: sha, FromSHA: sha, Reason: reason,
	})
	return true
}

func (pl *planner) ensureRepo(res *RefResult, p Platform) bool {
	exists, known := pl.in.RepoExists[p.Name]
	if known && !exists {
		if !p.Caps.RepoCreate && !p.Caps.GitPushCreateRepo {
			res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "capability", Text: "repository does not exist and push-to-create is not supported"})
			res.Blocked = "capability:create-repo"
			return false
		}
		if p.Caps.RepoCreate && !pl.created[p.Name] {
			pl.created[p.Name] = true
			res.Actions = append(res.Actions, Action{
				Kind: ActionCreateRepo, Platform: p.Name, Ref: res.Ref,
				Reason: "repository does not exist",
			})
		}
	}
	return true
}

func (pl *planner) snapshot(res *RefResult, p Platform, ref, sha, reason string) bool {
	if sha == "" {
		return false
	}
	if glob, reserved := p.Caps.Reserved(ref); reserved {
		res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "namespace", Text: "reserved namespace " + glob + ", snapshot skipped"})
		return false
	}
	if !p.Caps.GitPush {
		res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "capability", Text: "cannot write snapshot ref"})
		return false
	}
	res.Actions = append(res.Actions, Action{
		Kind: ActionSnapshot, Platform: p.Name, Ref: ref, FromSHA: sha,
		SnapshotRef: SnapshotRef(ref, pl.in.Now), Reason: reason,
	})
	return true
}

func (pl *planner) forcePush(res *RefResult, p Platform, ref, to, from string) bool {
	if glob, reserved := p.Caps.Reserved(ref); reserved {
		res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "namespace", Text: "reserved namespace " + glob})
		res.Blocked = "namespace:" + glob
		return false
	}
	if !p.Caps.GitPush || !p.Caps.GitPushForce {
		res.Notices = append(res.Notices, Notice{Platform: p.Name, Kind: "capability", Text: "platform refuses force-pushes"})
		res.Blocked = "capability:force-push"
		return false
	}
	if !pl.ensureRepo(res, p) {
		return false
	}
	if p.Caps.GitProtected && strings.HasPrefix(ref, "refs/heads/") {
		res.Actions = append(res.Actions,
			Action{Kind: ActionUnprotect, Platform: p.Name, Ref: ref, Reason: "temporary, same journalled group"},
		)
	}
	res.Actions = append(res.Actions, Action{
		Kind: ActionForcePush, Platform: p.Name, Ref: ref, ToSHA: to, LeaseSHA: from, FromSHA: from,
		Reason: "authority",
	})
	if p.Caps.GitProtected && strings.HasPrefix(ref, "refs/heads/") {
		res.Actions = append(res.Actions,
			Action{Kind: ActionReprotect, Platform: p.Name, Ref: ref, Reason: "restore protection in the same group"},
		)
	}
	return true
}

func (pl *planner) adjustBlocked(res *RefResult) {
	if res.Blocked == "" {
		return
	}
	if strings.HasPrefix(res.Blocked, "namespace:") {
		res.Class = ClassNamespaceRejected
	} else {
		res.Class = ClassCapabilityBlocked
	}
}

func fastForwardTip(distinct []string, rel Relations) (string, bool) {
	for _, candidate := range distinct {
		ok := true
		for _, other := range distinct {
			if other == candidate {
				continue
			}
			ancestor, known := rel.Ancestor(other, candidate)
			if !known || !ancestor {
				ok = false
				break
			}
		}
		if ok {
			return candidate, true
		}
	}
	return "", false
}

func mergeSides(authority string, present map[string]Observation, distinct []string) (string, string) {
	ours, theirs := distinct[0], distinct[1]
	if obs, ok := present[authority]; ok {
		ours = obs.SHA
		if distinct[1] == ours {
			theirs = distinct[0]
		}
	}
	return ours, theirs
}

func distinctSHAs(present map[string]Observation) []string {
	set := map[string]bool{}
	for _, obs := range present {
		set[obs.SHA] = true
	}
	out := make([]string, 0, len(set))
	for sha := range set {
		out = append(out, sha)
	}
	sort.Strings(out)
	return out
}

func priorChanged(prior map[string]Prior, present map[string]Observation) bool {
	for name, pr := range prior {
		if obs, ok := present[name]; ok && pr.SHA != "" && pr.SHA != obs.SHA {
			return true
		}
	}
	return false
}

func isTag(ref string) bool { return strings.HasPrefix(ref, "refs/tags/") }

func sortedPlatformNames(m map[string]Prior) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func shortList(shas []string) string {
	parts := make([]string, len(shas))
	for i, s := range shas {
		parts[i] = shortSHA(s)
	}
	return strings.Join(parts, ", ")
}
