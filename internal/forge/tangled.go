package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

type tangledAdapter struct {
	name string
	cfg  config.Platform
	api  *apiClient
	caps caps.Capabilities
}

func newTangled(name string, p config.Platform, token string, c caps.Capabilities, hc *http.Client) *tangledAdapter {
	base := p.Knot
	if base == "" {
		base = "tangled.org"
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return &tangledAdapter{name: name, cfg: p, api: newAPI(name, base, token, headers, hc), caps: c}
}

func (t *tangledAdapter) Name() string            { return t.name }
func (t *tangledAdapter) Kind() string            { return string(caps.KindTangled) }
func (t *tangledAdapter) Caps() caps.Capabilities { return t.caps }
func (t *tangledAdapter) Close() error            { return nil }

func (t *tangledAdapter) GitURL(owner, name string) string {
	base := t.cfg.Git
	if base == "" {
		knot := t.cfg.Knot
		if knot == "" {
			knot = "tangled.org"
		}
		if strings.Contains(knot, "://") {
			base = knot
		} else {
			base = "ssh://git@" + knot
		}
	}
	return JoinGitURL(base, owner, name)
}

func (t *tangledAdapter) GitEnv() []string {
	if ref, err := config.ParseAuthRef(t.cfg.Auth); err == nil && ref.Scheme == "ssh" {
		return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + SSHCommand(ref)}
	}
	if token := t.api.token; token != "" {
		return HTTPGitEnv(t.GitURL("x", "x"), "Bearer "+token)
	}
	return []string{"GIT_TERMINAL_PROMPT=0"}
}

func (t *tangledAdapter) Verify(ctx context.Context) error {
	var session map[string]any
	return t.api.do(ctx, http.MethodGet, "/xrpc/com.atproto.server.getSession", nil, &session)
}

type tangledRepoJSON struct {
	Name          string   `json:"name"`
	DID           string   `json:"did"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"defaultBranch"`
	Topics        []string `json:"topics"`
	Private       bool     `json:"private"`
}

func (r tangledRepoJSON) toRepo(owner string) Repo {
	v := "public"
	if r.Private {
		v = "private"
	}
	return Repo{Owner: owner, Name: r.Name, DID: r.DID, Description: r.Description,
		DefaultBranch: r.DefaultBranch, Topics: r.Topics, Visibility: v, Private: r.Private}
}

func (t *tangledAdapter) ListRepos(ctx context.Context) ([]Repo, error) {
	owner := t.cfg.Owner
	path := "/xrpc/org.tangled.repo.listRepos?limit=100"
	if owner != "" {
		path += "&owner=" + url.QueryEscape(owner)
	}
	var data struct {
		Repos []tangledRepoJSON `json:"repos"`
	}
	if err := t.api.do(ctx, http.MethodGet, path, nil, &data); err != nil {
		return nil, err
	}
	out := make([]Repo, 0, len(data.Repos))
	for _, r := range data.Repos {
		out = append(out, r.toRepo(owner))
	}
	return out, nil
}

func (t *tangledAdapter) GetRepo(ctx context.Context, owner, name string) (Repo, error) {
	path := "/xrpc/org.tangled.repo.getRepo?owner=" + url.QueryEscape(owner) + "&name=" + url.QueryEscape(name)
	var data struct {
		Repo *tangledRepoJSON `json:"repo"`
	}
	if err := t.api.do(ctx, http.MethodGet, path, nil, &data); err != nil {
		return Repo{}, err
	}
	if data.Repo == nil {
		return Repo{}, fmt.Errorf("tangled: repository %s/%s not found", owner, name)
	}
	return data.Repo.toRepo(owner), nil
}

func (t *tangledAdapter) CreateRepo(ctx context.Context, want Repo) (Repo, error) {
	did := want.DID
	if did == "" {
		did = t.cfg.Owner
	}
	body := map[string]any{
		"name":          want.Name,
		"did":           did,
		"defaultBranch": want.DefaultBranch,
		"description":   want.Description,
		"topics":        want.Topics,
	}
	if want.CloneURL != "" {
		body["source"] = want.CloneURL
	}
	var out tangledRepoJSON
	if err := t.api.do(ctx, http.MethodPost, "/xrpc/org.tangled.repo.createRepo", body, &out); err != nil {
		return Repo{}, err
	}
	got := out.toRepo(want.Owner)
	if got.Name == "" {
		got.Name = want.Name
	}
	if got.DID == "" {
		got.DID = did
	}
	return got, nil
}

func (t *tangledAdapter) SetMeta(ctx context.Context, owner, name string, m Meta) error {
	repo := owner
	if t.cfg.Owner != "" {
		repo = t.cfg.Owner
	}
	getPath := "/xrpc/com.atproto.repo.getRecord?repo=" + url.QueryEscape(repo) +
		"&collection=org.tangled.repo.repo&rkey=" + url.QueryEscape(name)
	var rec struct {
		Value map[string]any `json:"value"`
		CID   string         `json:"cid"`
	}
	if err := t.api.do(ctx, http.MethodGet, getPath, nil, &rec); err != nil {
		return err
	}
	if rec.Value == nil {
		rec.Value = map[string]any{}
	}
	if m.Description != nil {
		rec.Value["description"] = *m.Description
	}
	if m.Topics != nil {
		rec.Value["topics"] = *m.Topics
	}
	if m.DefaultBranch != nil {
		rec.Value["defaultBranch"] = *m.DefaultBranch
	}
	if m.Visibility != nil {
		rec.Value["private"] = *m.Visibility == "private"
	}
	body := map[string]any{
		"repo":       repo,
		"collection": "org.tangled.repo.repo",
		"rkey":       name,
		"record":     rec.Value,
	}
	return t.api.do(ctx, http.MethodPost, "/xrpc/com.atproto.repo.putRecord", body, nil)
}

func (t *tangledAdapter) Unprotect(context.Context, string, string, string) error {
	return ErrUnsupported
}

func (t *tangledAdapter) Reprotect(context.Context, string, string, string) error {
	return ErrUnsupported
}
