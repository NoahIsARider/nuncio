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

type gitlabAdapter struct {
	name string
	cfg  config.Platform
	api  *apiClient
	caps caps.Capabilities
	auth Auth
}

func newGitLab(name string, p config.Platform, token string, c caps.Capabilities, hc *http.Client) *gitlabAdapter {
	base := p.API
	if base == "" {
		base = "https://gitlab.com/api/v4"
	}
	headers := map[string]string{}
	if token != "" {
		headers["PRIVATE-TOKEN"] = token
	}
	return &gitlabAdapter{name: name, cfg: p, api: newAPI(name, base, token, headers, hc), caps: c, auth: Auth{Secret: token}}
}

func (g *gitlabAdapter) Name() string            { return g.name }
func (g *gitlabAdapter) Kind() string            { return string(caps.KindGitLab) }
func (g *gitlabAdapter) Caps() caps.Capabilities { return g.caps }
func (g *gitlabAdapter) Close() error            { return nil }

func (g *gitlabAdapter) GitURL(owner, name string) string {
	base := g.cfg.Git
	if base == "" {
		base = "ssh://git@gitlab.com"
	}
	return JoinGitURL(base, owner, name)
}

func (g *gitlabAdapter) GitEnv() []string {
	if ref, err := config.ParseAuthRef(g.cfg.Auth); err == nil && ref.Scheme == "ssh" {
		return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + SSHCommand(ref)}
	}
	if g.auth.Secret == "" {
		return nil
	}
	return HTTPGitEnv(g.GitURL("x", "x"), basicAuth("oauth2", g.auth.Secret))
}

func (g *gitlabAdapter) Verify(ctx context.Context) error {
	var user struct {
		Username string `json:"username"`
	}
	return g.api.do(ctx, http.MethodGet, "/user", nil, &user)
}

func projectID(owner, name string) string {
	return url.PathEscape(owner + "/" + name)
}

type glRepoJSON struct {
	ID                int64    `json:"id"`
	Name              string   `json:"name"`
	Path              string   `json:"path"`
	PathWithNamespace string   `json:"path_with_namespace"`
	Description       string   `json:"description"`
	Visibility        string   `json:"visibility"`
	DefaultBranch     string   `json:"default_branch"`
	Topics            []string `json:"topics"`
	HTTPURLToRepo     string   `json:"http_url_to_repo"`
	Namespace         struct {
		FullPath string `json:"full_path"`
	} `json:"namespace"`
}

func (r glRepoJSON) toRepo() Repo {
	return Repo{
		Owner: r.Namespace.FullPath, Name: r.Path, Description: r.Description,
		Visibility: r.Visibility, Private: r.Visibility == "private",
		DefaultBranch: r.DefaultBranch, Topics: r.Topics, CloneURL: r.HTTPURLToRepo,
	}
}

func (g *gitlabAdapter) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; page <= 100; page++ {
		var batch []glRepoJSON
		path := fmt.Sprintf("/projects?membership=true&per_page=100&page=%d", page)
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

func (g *gitlabAdapter) GetRepo(ctx context.Context, owner, name string) (Repo, error) {
	var r glRepoJSON
	if err := g.api.do(ctx, http.MethodGet, "/projects/"+projectID(owner, name), nil, &r); err != nil {
		return Repo{}, err
	}
	return r.toRepo(), nil
}

// namespaceID resolves an owner to a GitLab *group* ID. It deliberately does
// not fall back to the users endpoint: POST /projects only accepts a group ID
// in namespace_id, and a personal namespace must be expressed by omitting the
// field. Passing a user ID returns HTTP 400 {"namespace":["is not valid"]}.
func (g *gitlabAdapter) namespaceID(ctx context.Context, owner string) (int64, error) {
	var group struct {
		ID int64 `json:"id"`
	}
	if err := g.api.do(ctx, http.MethodGet, "/groups/"+url.PathEscape(owner), nil, &group); err == nil && group.ID != 0 {
		return group.ID, nil
	}
	return 0, nil
}

func (g *gitlabAdapter) CreateRepo(ctx context.Context, want Repo) (Repo, error) {
	visibility := want.Visibility
	if visibility == "" {
		if want.Private {
			visibility = "private"
		} else {
			visibility = "public"
		}
	}
	body := map[string]any{
		"name":                   want.Name,
		"path":                   want.Name,
		"description":            want.Description,
		"visibility":             visibility,
		"initialize_with_readme": false,
	}
	if ns, err := g.namespaceID(ctx, want.Owner); err == nil && ns != 0 {
		body["namespace_id"] = ns
	}
	var created glRepoJSON
	if err := g.api.do(ctx, http.MethodPost, "/projects", body, &created); err != nil {
		if IsConflict(err) {
			return g.GetRepo(ctx, want.Owner, want.Name)
		}
		return Repo{}, err
	}
	// GitLab creates projects private in some paths regardless of source
	// visibility. Read the result back and correct it explicitly.
	got := created.toRepo()
	if visibility != "" && got.Visibility != visibility {
		if err := g.api.do(ctx, http.MethodPut, fmt.Sprintf("/projects/%d", created.ID), map[string]any{"visibility": visibility}, nil); err != nil {
			return Repo{}, err
		}
		got.Visibility = visibility
		got.Private = visibility == "private"
	}
	return got, nil
}

func (g *gitlabAdapter) SetMeta(ctx context.Context, owner, name string, m Meta) error {
	body := map[string]any{}
	if m.Description != nil {
		body["description"] = *m.Description
	}
	if m.Visibility != nil {
		body["visibility"] = *m.Visibility
	}
	if m.DefaultBranch != nil {
		body["default_branch"] = *m.DefaultBranch
	}
	if m.Topics != nil {
		body["topics"] = strings.Join(*m.Topics, ",")
	}
	if len(body) == 0 {
		return nil
	}
	return g.api.do(ctx, http.MethodPut, "/projects/"+projectID(owner, name), body, nil)
}

func (g *gitlabAdapter) resolveProjectID(ctx context.Context, owner, name string) (int64, error) {
	var r struct {
		ID int64 `json:"id"`
	}
	if err := g.api.do(ctx, http.MethodGet, "/projects/"+projectID(owner, name), nil, &r); err != nil {
		return 0, err
	}
	return r.ID, nil
}

func (g *gitlabAdapter) Unprotect(ctx context.Context, owner, name, branch string) error {
	id, err := g.resolveProjectID(ctx, owner, name)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/projects/%d/protected_branches/%s", id, url.PathEscape(branch))
	err = g.api.do(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

func (g *gitlabAdapter) Reprotect(ctx context.Context, owner, name, branch string) error {
	id, err := g.resolveProjectID(ctx, owner, name)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/projects/%d/protected_branches?name=%s&push_access_level=40&merge_access_level=40",
		id, url.QueryEscape(branch))
	return g.api.do(ctx, http.MethodPost, path, nil, nil)
}
