package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

type forgejoAdapter struct {
	name string
	cfg  config.Platform
	api  *apiClient
	caps caps.Capabilities
	auth Auth
}

func newForgejo(name string, p config.Platform, token string, c caps.Capabilities, hc *http.Client) *forgejoAdapter {
	base := p.API
	if base == "" {
		base = "https://codeberg.org/api/v1"
	}
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "token " + token
	}
	return &forgejoAdapter{name: name, cfg: p, api: newAPI(name, base, token, headers, hc), caps: c, auth: Auth{Secret: token}}
}

func (f *forgejoAdapter) Name() string            { return f.name }
func (f *forgejoAdapter) Kind() string            { return string(caps.KindForgejo) }
func (f *forgejoAdapter) Caps() caps.Capabilities { return f.caps }
func (f *forgejoAdapter) Close() error            { return nil }

func (f *forgejoAdapter) GitURL(owner, name string) string {
	base := f.cfg.Git
	if base == "" {
		base = "ssh://git@codeberg.org"
	}
	return JoinGitURL(base, owner, name)
}

func (f *forgejoAdapter) GitEnv() []string {
	if ref, err := config.ParseAuthRef(f.cfg.Auth); err == nil && ref.Scheme == "ssh" {
		return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + SSHCommand(ref)}
	}
	if f.auth.Secret == "" {
		return nil
	}
	return HTTPGitEnv(f.GitURL("x", "x"), basicAuth("nuncio", f.auth.Secret))
}

func (f *forgejoAdapter) Verify(ctx context.Context) error {
	var user struct {
		Login string `json:"login"`
	}
	return f.api.do(ctx, http.MethodGet, "/user", nil, &user)
}

type fjRepoJSON struct {
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Description   string   `json:"description"`
	Private       bool     `json:"private"`
	DefaultBranch string   `json:"default_branch"`
	Topics        []string `json:"topics"`
	CloneURL      string   `json:"clone_url"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r fjRepoJSON) toRepo() Repo {
	v := "public"
	if r.Private {
		v = "private"
	}
	return Repo{
		Owner: r.Owner.Login, Name: r.Name, Description: r.Description, Private: r.Private,
		Visibility: v, DefaultBranch: r.DefaultBranch, Topics: r.Topics, CloneURL: r.CloneURL,
	}
}

func (f *forgejoAdapter) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; page <= 100; page++ {
		var batch []fjRepoJSON
		path := fmt.Sprintf("/user/repos?limit=50&page=%d", page)
		if err := f.api.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return out, err
		}
		for _, r := range batch {
			out = append(out, r.toRepo())
		}
		if len(batch) < 50 {
			break
		}
	}
	return out, nil
}

func (f *forgejoAdapter) GetRepo(ctx context.Context, owner, name string) (Repo, error) {
	var r fjRepoJSON
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	if err := f.api.do(ctx, http.MethodGet, path, nil, &r); err != nil {
		return Repo{}, err
	}
	return r.toRepo(), nil
}

func (f *forgejoAdapter) CreateRepo(ctx context.Context, want Repo) (Repo, error) {
	endpoint := "/user/repos"
	var org struct {
		ID int64 `json:"id"`
	}
	if err := f.api.do(ctx, http.MethodGet, "/orgs/"+url.PathEscape(want.Owner), nil, &org); err == nil && org.ID != 0 {
		endpoint = "/orgs/" + url.PathEscape(want.Owner) + "/repos"
	}
	body := map[string]any{
		"name":           want.Name,
		"private":        want.Private || want.Visibility == "private",
		"description":    want.Description,
		"default_branch": want.DefaultBranch,
		"auto_init":      false,
		"topics":         want.Topics,
	}
	var created fjRepoJSON
	if err := f.api.do(ctx, http.MethodPost, endpoint, body, &created); err != nil {
		if IsConflict(err) || IsNotFound(err) {
			return f.GetRepo(ctx, want.Owner, want.Name)
		}
		return Repo{}, err
	}
	return created.toRepo(), nil
}

func (f *forgejoAdapter) SetMeta(ctx context.Context, owner, name string, m Meta) error {
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
	if m.Topics != nil {
		body["topics"] = *m.Topics
	}
	if len(body) == 0 {
		return nil
	}
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	return f.api.do(ctx, http.MethodPatch, path, body, nil)
}

func (f *forgejoAdapter) Unprotect(ctx context.Context, owner, name, branch string) error {
	var protections []struct {
		Name string `json:"branch_name"`
	}
	base := fmt.Sprintf("/repos/%s/%s/branch_protections", url.PathEscape(owner), url.PathEscape(name))
	if err := f.api.do(ctx, http.MethodGet, base, nil, &protections); err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, p := range protections {
		if p.Name != branch {
			continue
		}
		if err := f.api.do(ctx, http.MethodDelete, base+"/"+url.PathEscape(branch), nil, nil); err != nil && !IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (f *forgejoAdapter) Reprotect(ctx context.Context, owner, name, branch string) error {
	base := fmt.Sprintf("/repos/%s/%s/branch_protections", url.PathEscape(owner), url.PathEscape(name))
	body := map[string]any{
		"branch_name":           branch,
		"rule_name":             branch,
		"enable_push":           true,
		"enable_force_push":     false,
		"enable_push_whitelist": false,
	}
	err := f.api.do(ctx, http.MethodPost, base, body, nil)
	if err != nil && IsConflict(err) {
		return nil
	}
	return err
}
