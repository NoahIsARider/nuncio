package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

type githubAdapter struct {
	name string
	cfg  config.Platform
	api  *apiClient
	caps caps.Capabilities
	auth Auth
}

func newGitHub(name string, p config.Platform, token string, c caps.Capabilities, hc *http.Client) *githubAdapter {
	base := p.API
	if base == "" {
		base = "https://api.github.com"
	}
	headers := map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return &githubAdapter{name: name, cfg: p, api: newAPI(name, base, token, headers, hc), caps: c, auth: Auth{Secret: token}}
}

func (g *githubAdapter) Name() string            { return g.name }
func (g *githubAdapter) Kind() string            { return string(caps.KindGitHub) }
func (g *githubAdapter) Caps() caps.Capabilities { return g.caps }
func (g *githubAdapter) Close() error            { return nil }

func (g *githubAdapter) GitURL(owner, name string) string {
	base := g.cfg.Git
	if base == "" {
		base = "https://github.com"
	}
	return JoinGitURL(base, owner, name)
}

func (g *githubAdapter) GitEnv() []string {
	if ref, err := config.ParseAuthRef(g.cfg.Auth); err == nil && ref.Scheme == "ssh" {
		return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + SSHCommand(ref)}
	}
	if g.auth.Secret == "" {
		return nil
	}
	return HTTPGitEnv(g.GitURL("x", "x"), basicAuth("x-access-token", g.auth.Secret))
}

func (g *githubAdapter) Verify(ctx context.Context) error {
	var user struct {
		Login string `json:"login"`
	}
	return g.api.do(ctx, http.MethodGet, "/user", nil, &user)
}

type ghRepoJSON struct {
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	Topics        []string `json:"topics"`
	CloneURL      string   `json:"clone_url"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	Visibility string `json:"visibility"`
}

func (r ghRepoJSON) toRepo() Repo {
	visibility := r.Visibility
	if visibility == "" {
		if r.Private {
			visibility = "private"
		} else {
			visibility = "public"
		}
	}
	return Repo{
		Owner: r.Owner.Login, Name: r.Name, Private: r.Private, Visibility: visibility,
		Description: r.Description, DefaultBranch: r.DefaultBranch, Topics: r.Topics, CloneURL: r.CloneURL,
	}
}

func (g *githubAdapter) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; page <= 100; page++ {
		var batch []ghRepoJSON
		path := fmt.Sprintf("/user/repos?per_page=100&affiliation=owner,collaborator,organization_member&page=%d", page)
		if err := g.api.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return out, err
		}
		for _, r := range batch {
			out = append(out, r.toRepo())
		}
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

func (g *githubAdapter) GetRepo(ctx context.Context, owner, name string) (Repo, error) {
	var r ghRepoJSON
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	if err := g.api.do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return Repo{}, err
	}
	return r.toRepo(), nil
}

func (g *githubAdapter) CreateRepo(ctx context.Context, want Repo) (Repo, error) {
	var user struct {
		Type string `json:"type"`
	}
	endpoint := "/user/repos"
	if err := g.api.do(ctx, http.MethodGet, "/users/"+url.PathEscape(want.Owner), nil, &user); err == nil && user.Type == "Organization" {
		endpoint = "/orgs/" + url.PathEscape(want.Owner) + "/repos"
	}
	body := map[string]any{
		"name":           want.Name,
		"private":        want.Private || want.Visibility == "private",
		"description":    want.Description,
		"default_branch": want.DefaultBranch,
		"auto_init":      false,
	}
	var created ghRepoJSON
	if err := g.api.do(ctx, http.MethodPost, endpoint, body, &created); err != nil {
		if IsNotFound(err) || IsConflict(err) {
			return g.GetRepo(ctx, want.Owner, want.Name)
		}
		return Repo{}, err
	}
	return created.toRepo(), nil
}

func (g *githubAdapter) SetMeta(ctx context.Context, owner, name string, m Meta) error {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	body := map[string]any{}
	if m.Description != nil {
		body["description"] = *m.Description
	}
	if m.Visibility != nil {
		body["private"] = *m.Visibility == "private"
	}
	if m.DefaultBranch != nil {
		body["default_branch"] = *m.DefaultBranch
	}
	if len(body) > 0 {
		if err := g.api.do(ctx, http.MethodPatch, path, body, nil); err != nil {
			return err
		}
	}
	if m.Topics != nil {
		if err := g.api.do(ctx, http.MethodPut, path+"/topics", map[string]any{"names": *m.Topics}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (g *githubAdapter) Unprotect(ctx context.Context, owner, name, branch string) error {
	path := fmt.Sprintf("/repos/%s/%s/branches/%s/protection",
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(branch))
	err := g.api.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

func (g *githubAdapter) Reprotect(ctx context.Context, owner, name, branch string) error {
	path := fmt.Sprintf("/repos/%s/%s/branches/%s/protection",
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(branch))
	body := map[string]any{
		"required_status_checks":        nil,
		"enforce_admins":                false,
		"required_pull_request_reviews": nil,
		"restrictions":                  nil,
	}
	return g.api.do(ctx, http.MethodPut, path, body, nil)
}
