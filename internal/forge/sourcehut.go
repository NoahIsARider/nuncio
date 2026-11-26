package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

type sourcehutAdapter struct {
	name string
	cfg  config.Platform
	api  *apiClient
	caps caps.Capabilities
}

func newSourceHut(name string, p config.Platform, token string, c caps.Capabilities, hc *http.Client) *sourcehutAdapter {
	base := p.API
	if base == "" {
		base = "https://git.sr.ht/query"
	}
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return &sourcehutAdapter{name: name, cfg: p, api: newAPI(name, base, token, headers, hc), caps: c}
}

func (s *sourcehutAdapter) Name() string            { return s.name }
func (s *sourcehutAdapter) Kind() string            { return string(caps.KindSourceHut) }
func (s *sourcehutAdapter) Caps() caps.Capabilities { return s.caps }
func (s *sourcehutAdapter) Close() error            { return nil }

func (s *sourcehutAdapter) GitURL(owner, name string) string {
	base := s.cfg.Git
	if base == "" {
		base = "ssh://git@git.sr.ht"
	}
	return JoinGitURL(base, owner, name)
}

func (s *sourcehutAdapter) GitEnv() []string {
	if ref, err := config.ParseAuthRef(s.cfg.Auth); err == nil && ref.Scheme == "ssh" {
		return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + SSHCommand(ref)}
	}
	return []string{"GIT_TERMINAL_PROMPT=0"}
}

func (s *sourcehutAdapter) gql(ctx context.Context, doc string, vars map[string]any, out any) error {
	body := map[string]any{"query": doc}
	if len(vars) > 0 {
		body["variables"] = vars
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := s.api.do(ctx, http.MethodPost, "", body, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("sourcehut graphql: %s", strings.Join(msgs, ", "))
	}
	if out != nil && len(envelope.Data) > 0 {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

func (s *sourcehutAdapter) Verify(ctx context.Context) error {
	var data struct {
		Me *struct {
			CanonicalName string `json:"canonicalName"`
			Username      string `json:"username"`
		} `json:"me"`
	}
	if err := s.gql(ctx, `query { me { canonicalName username } }`, nil, &data); err != nil {
		return err
	}
	if data.Me == nil {
		return fmt.Errorf("sourcehut: no authenticated user")
	}
	return nil
}

func (s *sourcehutAdapter) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	cursor := ""
	for i := 0; i < 100; i++ {
		var data struct {
			Me *struct {
				Repositories struct {
					Cursor  *string `json:"cursor"`
					Results []struct {
						Name        string `json:"name"`
						Visibility  string `json:"visibility"`
						Description string `json:"description"`
					} `json:"results"`
				} `json:"repositories"`
			} `json:"me"`
		}
		vars := map[string]any{}
		doc := `query($cursor: Cursor) { me { repositories(cursor: $cursor) { cursor results { name visibility description } } } }`
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := s.gql(ctx, doc, vars, &data); err != nil {
			return out, err
		}
		if data.Me == nil {
			break
		}
		for _, r := range data.Me.Repositories.Results {
			out = append(out, Repo{
				Owner: s.cfg.Owner, Name: r.Name, Visibility: strings.ToLower(r.Visibility),
				Private: strings.EqualFold(r.Visibility, "PRIVATE"), Description: r.Description,
			})
		}
		if data.Me.Repositories.Cursor == nil || *data.Me.Repositories.Cursor == "" {
			break
		}
		cursor = *data.Me.Repositories.Cursor
	}
	return out, nil
}

func (s *sourcehutAdapter) GetRepo(ctx context.Context, owner, name string) (Repo, error) {
	var data struct {
		Repository *struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Visibility  string `json:"visibility"`
			Description string `json:"description"`
		} `json:"repository"`
	}
	doc := `query($name: String, $owner: String) { repository(name: $name, owner: $owner) { id name visibility description } }`
	if err := s.gql(ctx, doc, map[string]any{"name": name, "owner": owner}, &data); err != nil {
		return Repo{}, err
	}
	if data.Repository == nil {
		return Repo{}, fmt.Errorf("sourcehut: repository %s/%s not found", owner, name)
	}
	vis := strings.ToLower(data.Repository.Visibility)
	return Repo{Owner: owner, Name: data.Repository.Name, Visibility: vis, Private: vis == "private", Description: data.Repository.Description}, nil
}

func (s *sourcehutAdapter) CreateRepo(ctx context.Context, want Repo) (Repo, error) {
	visibility := strings.ToUpper(want.Visibility)
	if visibility == "" {
		if want.Private {
			visibility = "PRIVATE"
		} else {
			visibility = "PUBLIC"
		}
	}
	doc := `mutation($name: String!, $visibility: Visibility!, $description: String, $cloneUrl: String) {
	  createRepository(name: $name, visibility: $visibility, description: $description, cloneUrl: $cloneUrl) { id name }
	}`
	vars := map[string]any{"name": want.Name, "visibility": visibility, "description": want.Description}
	if want.CloneURL != "" {
		vars["cloneUrl"] = want.CloneURL
	}
	var data struct {
		Create *struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"createRepository"`
	}
	if err := s.gql(ctx, doc, vars, &data); err != nil {
		if existing, getErr := s.GetRepo(ctx, want.Owner, want.Name); getErr == nil {
			return existing, nil
		}
		return Repo{}, err
	}
	want.Visibility = strings.ToLower(visibility)
	want.Private = visibility == "PRIVATE"
	return want, nil
}

func (s *sourcehutAdapter) SetMeta(ctx context.Context, owner, name string, m Meta) error {
	var data struct {
		Repository *struct {
			ID int64 `json:"id"`
		} `json:"repository"`
	}
	doc := `query($name: String, $owner: String) { repository(name: $name, owner: $owner) { id } }`
	if err := s.gql(ctx, doc, map[string]any{"name": name, "owner": owner}, &data); err != nil {
		return err
	}
	if data.Repository == nil {
		return fmt.Errorf("sourcehut: repository %s/%s not found", owner, name)
	}
	input := map[string]any{}
	if m.Description != nil {
		input["description"] = *m.Description
	}
	if m.Visibility != nil {
		input["visibility"] = strings.ToUpper(*m.Visibility)
	}
	if m.DefaultBranch != nil {
		input["HEAD"] = *m.DefaultBranch
	}
	if len(input) == 0 {
		return nil
	}
	mut := `mutation($id: Int!, $input: RepoInput!) { updateRepository(id: $id, input: $input) { id } }`
	return s.gql(ctx, mut, map[string]any{"id": data.Repository.ID, "input": input}, nil)
}

func (s *sourcehutAdapter) Unprotect(context.Context, string, string, string) error {
	return ErrUnsupported
}

func (s *sourcehutAdapter) Reprotect(context.Context, string, string, string) error {
	return ErrUnsupported
}
