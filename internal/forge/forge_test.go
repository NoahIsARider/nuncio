package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

func newAdapter(t *testing.T, kind string, handler http.Handler) (Adapter, *config.Platform) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := config.Platform{Kind: kind, API: srv.URL, Git: "https://example.invalid"}
	if kind == string(caps.KindSourceHut) {
		p.API = srv.URL + "/query"
		p.Owner = "~alice"
	}
	if kind == string(caps.KindTangled) {
		p.API = ""
		p.Knot = srv.URL
		p.Owner = "alice.tngl.sh"
	}
	a, err := New(context.Background(), "test", p, Auth{Ref: config.AuthRef{Scheme: "env", Value: "TOKEN"}, Secret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	return a, &p
}

func TestGitHubAdapter(t *testing.T) {
	mux := http.NewServeMux()
	var sawTopics bool
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer s3cret" {
			t.Errorf("authorization = %q", got)
		}
		io.WriteString(w, `{"login":"alice"}`)
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "newrepo" || body["private"] != true {
				t.Errorf("create body = %v", body)
			}
			io.WriteString(w, `{"name":"newrepo","private":true,"visibility":"private","owner":{"login":"alice"}}`)
			return
		}
		io.WriteString(w, `[{"name":"example","private":false,"owner":{"login":"alice"},"default_branch":"main","topics":["x"]}]`)
	})
	mux.HandleFunc("/users/alice", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"type":"User"}`)
	})
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"repo","private":false,"owner":{"login":"acme"}}`)
	})
	mux.HandleFunc("/repos/alice/example", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["description"] != "hi" {
				t.Errorf("patch body = %v", body)
			}
		}
		io.WriteString(w, `{"name":"example","private":false,"owner":{"login":"alice"},"default_branch":"main"}`)
	})
	mux.HandleFunc("/repos/alice/example/topics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("topics method = %s", r.Method)
		}
		sawTopics = true
		io.WriteString(w, `{"names":["a"]}`)
	})
	mux.HandleFunc("/repos/alice/example/branches/main/protection", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPut {
			t.Errorf("protection method = %s", r.Method)
		}
		io.WriteString(w, `{}`)
	})
	a, _ := newAdapter(t, "github", mux)
	ctx := context.Background()
	if err := a.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	repos, err := a.ListRepos(ctx)
	if err != nil || len(repos) != 1 || repos[0].Owner != "alice" {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	created, err := a.CreateRepo(ctx, Repo{Owner: "alice", Name: "newrepo", Private: true})
	if err != nil || created.Name != "newrepo" || !created.Private {
		t.Fatalf("created = %+v, %v", created, err)
	}
	desc := "hi"
	if err := a.SetMeta(ctx, "alice", "example", Meta{Description: &desc, Topics: &[]string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if !sawTopics {
		t.Error("topics were not written")
	}
	if err := a.Unprotect(ctx, "alice", "example", "main"); err != nil {
		t.Errorf("404 protection should be tolerated: %v", err)
	}
	if err := a.Reprotect(ctx, "alice", "example", "main"); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(a.GitEnv(), "\n")
	if !strings.Contains(env, "GIT_CONFIG_KEY_0=http.https://example.invalid/.extraheader") {
		t.Errorf("git env = %s", env)
	}
	if strings.Contains(a.GitURL("a", "b"), "s3cret") {
		t.Error("token leaked into a URL")
	}
}

func TestGitLabAdapter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "s3cret" {
			t.Errorf("token header = %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		io.WriteString(w, `{"username":"alice"}`)
	})
	mux.HandleFunc("/projects", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["visibility"] != "public" {
			t.Errorf("create visibility = %v", body["visibility"])
		}
		if body["namespace_id"] != float64(7) {
			t.Errorf("namespace = %v", body["namespace_id"])
		}
		io.WriteString(w, `{"id":1,"name":"example","path":"example","visibility":"private","namespace":{"full_path":"acme"}}`)
	})
	mux.HandleFunc("/projects/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("visibility method = %s", r.Method)
		}
		io.WriteString(w, `{"id":1,"visibility":"public"}`)
	})
	mux.HandleFunc("/groups/acme", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":7}`)
	})
	mux.HandleFunc("/projects/acme%2Fexample", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":1,"name":"example","path":"example","visibility":"public","namespace":{"full_path":"acme"}}`)
	})
	mux.HandleFunc("/projects/1/protected_branches/main", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/projects/1/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Query().Get("name") != "main" {
			t.Errorf("reprotect = %s %s", r.Method, r.URL.String())
		}
		io.WriteString(w, `{}`)
	})
	a, _ := newAdapter(t, "gitlab", mux)
	ctx := context.Background()
	if err := a.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := a.CreateRepo(ctx, Repo{Owner: "acme", Name: "example", Visibility: "public"})
	if err != nil || created.Visibility != "public" {
		t.Fatalf("created = %+v, %v", created, err)
	}
	if err := a.Unprotect(ctx, "acme", "example", "main"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reprotect(ctx, "acme", "example", "main"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(a.GitEnv(), "\n"), "Basic ") {
		t.Error("gitlab git env lacks basic auth")
	}
}

func TestForgejoAdapter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token s3cret" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"login":"alice"}`)
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "new" {
				t.Errorf("body = %v", body)
			}
			io.WriteString(w, `{"name":"new","private":true,"owner":{"login":"alice"}}`)
			return
		}
		io.WriteString(w, `[{"name":"example","private":false,"owner":{"login":"alice"},"default_branch":"main"}]`)
	})
	mux.HandleFunc("/orgs/acme", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/alice/example", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"example","private":false,"owner":{"login":"alice"}}`)
	})
	mux.HandleFunc("/repos/alice/example/branch_protections", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `[{"branch_name":"main"}]`)
			return
		}
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/repos/alice/example/branch_protections/main", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	a, _ := newAdapter(t, "forgejo", mux)
	ctx := context.Background()
	if err := a.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	repos, err := a.ListRepos(ctx)
	if err != nil || len(repos) != 1 {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	created, err := a.CreateRepo(ctx, Repo{Owner: "alice", Name: "new", Private: true})
	if err != nil || !created.Private {
		t.Fatalf("created = %+v, %v", created, err)
	}
	if err := a.Unprotect(ctx, "alice", "example", "main"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reprotect(ctx, "alice", "example", "main"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Caps().Reserved("refs/pull/1/head"); !ok {
		t.Error("forgejo should reserve refs/pull/*")
	}
}

func TestSourceHutAdapter(t *testing.T) {
	mux := http.NewServeMux()
	var lastQuery string
	mux.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		lastQuery = body.Query
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "createRepository"):
			io.WriteString(w, `{"data":{"createRepository":{"id":1,"name":"new"}}}`)
		case strings.Contains(body.Query, "updateRepository"):
			io.WriteString(w, `{"data":{"updateRepository":{"id":1}}}`)
		case strings.Contains(body.Query, "repositories("):
			io.WriteString(w, `{"data":{"me":{"repositories":{"cursor":null,"results":[{"name":"example","visibility":"PUBLIC","description":"d"}]}}}}`)
		case strings.Contains(body.Query, "repository("):
			io.WriteString(w, `{"data":{"repository":{"id":1,"name":"example","visibility":"PUBLIC","description":"d"}}}`)
		default:
			io.WriteString(w, `{"data":{"me":{"canonicalName":"alice"}}}`)
		}
	})
	a, _ := newAdapter(t, "sourcehut", mux)
	ctx := context.Background()
	if err := a.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	repos, err := a.ListRepos(ctx)
	if err != nil || len(repos) != 1 || repos[0].Name != "example" {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	if _, err := a.CreateRepo(ctx, Repo{Name: "new", Visibility: "public", CloneURL: "https://example/x"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastQuery, "createRepository") {
		t.Errorf("last query = %s", lastQuery)
	}
	desc := "new description"
	if err := a.SetMeta(ctx, "~alice", "example", Meta{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if err := a.Unprotect(ctx, "~alice", "example", "main"); err != ErrUnsupported {
		t.Errorf("sourcehut unprotect = %v", err)
	}
}

func TestTangledAdapter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.getSession", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"did":"did:plc:alice"}`)
	})
	mux.HandleFunc("/xrpc/org.tangled.repo.createRepo", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["did"] != "alice.tngl.sh" || body["name"] != "new" {
			t.Errorf("body = %v", body)
		}
		io.WriteString(w, `{"name":"new","did":"alice.tngl.sh"}`)
	})
	mux.HandleFunc("/xrpc/org.tangled.repo.getRepo", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "new" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"repo":{"name":"new","did":"alice.tngl.sh"}}`)
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.getRecord", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"value":{"description":"old","topics":["x"]}}`)
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.putRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Record map[string]any `json:"record"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Record["description"] != "new" {
			t.Errorf("record = %v", body.Record)
		}
		io.WriteString(w, `{}`)
	})
	a, _ := newAdapter(t, "tangled", mux)
	ctx := context.Background()
	if err := a.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := a.CreateRepo(ctx, Repo{Owner: "alice.tngl.sh", Name: "new", DID: "alice.tngl.sh", DefaultBranch: "main"})
	if err != nil || created.Name != "new" {
		t.Fatalf("created = %+v, %v", created, err)
	}
	desc := "new"
	if err := a.SetMeta(ctx, "alice.tngl.sh", "new", Meta{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Caps().Reserved("refs/atproto/x"); !ok {
		t.Error("tangled should reserve refs/atproto/*")
	}
	if _, ok := a.Caps().Reserved("refs/cobs/x"); !ok {
		t.Error("tangled should reserve refs/cobs/*")
	}
}

func TestPlainGitAdapter(t *testing.T) {
	p := config.Platform{Kind: "git", Git: "file:///D:/repos"}
	a, err := New(context.Background(), "local", p, Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.GitURL("alice", "example"); got != "file:///D:/repos/alice/example.git" {
		t.Errorf("git url = %s", got)
	}
	if err := a.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateRepo(context.Background(), Repo{}); err != ErrUnsupported {
		t.Errorf("create = %v", err)
	}
	got, err := a.GetRepo(context.Background(), "alice", "example")
	if err != nil || got.Name != "example" {
		t.Errorf("get = %+v, %v", got, err)
	}
}

func TestJoinGitURL(t *testing.T) {
	cases := []struct{ base, owner, name, want string }{
		{"https://github.com", "a", "b", "https://github.com/a/b.git"},
		{"https://github.com/", "a", "b", "https://github.com/a/b.git"},
		{"ssh://git@codeberg.org", "a", "b", "ssh://git@codeberg.org/a/b.git"},
		{"file:///D:/repos", "a", "b", "file:///D:/repos/a/b.git"},
		{"", "a", "b", "a/b.git"},
	}
	for _, tc := range cases {
		if got := JoinGitURL(tc.base, tc.owner, tc.name); got != tc.want {
			t.Errorf("JoinGitURL(%q,%q,%q)=%q want %q", tc.base, tc.owner, tc.name, got, tc.want)
		}
	}
}
