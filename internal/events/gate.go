package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/NoahIsARider/nuncio/internal/state"
)

// GateEvent is the minimum a pre-receive hook reports.
type GateEvent struct {
	Ref    string `json:"ref"`
	OldSHA string `json:"old"`
	NewSHA string `json:"new"`
	User   string `json:"user,omitempty"`
}

// Gate implements the SourceHut pre-receive warning path. S1 (whether a
// third-party operator can actually reject a push) is unverified, so the gate
// never blocks. It names the peer tip and the exact command to inspect it,
// which is materially better than reconciling silently after the fact.
type Gate struct {
	Store *state.Store
	// Peers reports the currently observed tip of ref on every other platform.
	Peers func(ctx context.Context, slug, ref string) (map[string]string, error)
	Logf  func(format string, args ...any)
}

// Check decides what to tell the pusher. blocked is always false today; the
// signature keeps the enforcement path explicit for when S1 is answered.
func (g *Gate) Check(ctx context.Context, slug string, ev GateEvent) (message string, blocked bool) {
	if ev.Ref == "" || ev.NewSHA == "" {
		return "", false
	}
	if g.Peers == nil {
		return "", false
	}
	peers, err := g.Peers(ctx, slug, ev.Ref)
	if err != nil {
		g.Logf("gate: cannot read peer tips for %s %s: %v", slug, ev.Ref, err)
		return "", false
	}
	var conflicts []string
	for platform, sha := range peers {
		if sha != "" && sha != ev.NewSHA && sha != ev.OldSHA {
			conflicts = append(conflicts, fmt.Sprintf("%s %s", platform, shortSHA(sha)))
		}
	}
	if len(conflicts) == 0 {
		return "", false
	}
	msg := fmt.Sprintf(
		"nuncio: this push would diverge %s %s from %s. It is NOT blocked (the sr.ht gate is unverified), but the peer tip is preserved.\n"+
			"  inspect with: nuncio report --repo %s\n"+
			"  the other tip will be snapshotted to refs/nuncio/snapshot/ before any reconcile overwrites anything.",
		slug, ev.Ref, strings.Join(conflicts, ", "), slug)
	g.Logf("gate: warning for %s %s: peer tips %s", slug, ev.Ref, strings.Join(conflicts, ", "))
	return msg, false
}

// Handler exposes the gate over HTTP for a local git.sr.ht-update-hook style
// caller. It accepts JSON or form fields and always answers 200; the response
// body is the warning text, which git prints to the pusher.
func (g *Gate) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(strings.TrimPrefix(req.URL.Path, "/gate/"), "/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		platform := req.URL.Query().Get("platform")
		if platform == "" {
			platform = "srht"
		}
		repo, err := g.Store.FindRepoByRemote(req.Context(), platform, parts[0], parts[1])
		if err != nil {
			http.Error(w, "repository is not managed by nuncio", http.StatusNotFound)
			return
		}
		var ev GateEvent
		if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&ev); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
		} else {
			_ = req.ParseForm()
			ev.Ref = req.FormValue("ref")
			ev.OldSHA = req.FormValue("old")
			ev.NewSHA = req.FormValue("new")
			ev.User = req.FormValue("user")
		}
		msg, _ := g.Check(req.Context(), repo.Slug, ev)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if msg == "" {
			fmt.Fprintln(w, "ok")
			return
		}
		fmt.Fprintln(w, msg)
	})
}

// GateFromStore is a convenience constructor using the state store for peer
// coordinates, but peers still have to be supplied by the caller because only
// the daemon knows how to read the forges.
func GateFromStore(store *state.Store, peers func(context.Context, string, string) (map[string]string, error)) *Gate {
	return &Gate{Store: store, Peers: peers, Logf: func(string, ...any) {}}
}

// ReceiverFromConfig builds a receiver's platform map from resolved values.
func ReceiverFromConfig(states map[string]WebhookPlatform) map[string]WebhookPlatform {
	return states
}

// NowUnix is a tiny helper for tests that want a deterministic clock.
func NowUnix(t time.Time) int64 { return t.Unix() }

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
