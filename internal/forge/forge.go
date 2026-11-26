// Package forge adapts each platform's API. The adapters do not decide
// anything about sync semantics; they report capabilities, read repository
// metadata, and create or protect repositories. Ref movement always goes
// through git, and every push is verified by the caller.
package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/config"
)

// ErrUnsupported means the platform has no endpoint for this operation.
var ErrUnsupported = errors.New("operation unsupported on this platform")

// Repo is a platform repository's metadata.
type Repo struct {
	Owner         string   `json:"owner"`
	Name          string   `json:"name"`
	DID           string   `json:"did,omitempty"`
	Description   string   `json:"description,omitempty"`
	Visibility    string   `json:"visibility,omitempty"`
	Private       bool     `json:"private,omitempty"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	Topics        []string `json:"topics,omitempty"`
	CloneURL      string   `json:"clone_url,omitempty"`
}

// Meta is a partial metadata update.
type Meta struct {
	Description   *string
	Visibility    *string
	Topics        *[]string
	DefaultBranch *string
}

// Adapter is one configured platform.
type Adapter interface {
	Name() string
	Kind() string
	Caps() caps.Capabilities
	GitURL(owner, name string) string
	GitEnv() []string
	Verify(ctx context.Context) error
	ListRepos(ctx context.Context) ([]Repo, error)
	CreateRepo(ctx context.Context, r Repo) (Repo, error)
	GetRepo(ctx context.Context, owner, name string) (Repo, error)
	SetMeta(ctx context.Context, owner, name string, m Meta) error
	Unprotect(ctx context.Context, owner, name, branch string) error
	Reprotect(ctx context.Context, owner, name, branch string) error
	Close() error
}

// Auth is a resolved credential. Secret is empty for SSH aliases.
type Auth struct {
	Ref    config.AuthRef
	Secret string
}

// New builds the adapter for a platform config.
func New(ctx context.Context, name string, p config.Platform, auth Auth) (Adapter, error) {
	c, err := caps.Declare(p.Kind)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 45 * time.Second}
	switch p.Kind {
	case string(caps.KindGitHub):
		return newGitHub(name, p, auth.Secret, c, hc), nil
	case string(caps.KindGitLab):
		return newGitLab(name, p, auth.Secret, c, hc), nil
	case string(caps.KindForgejo):
		return newForgejo(name, p, auth.Secret, c, hc), nil
	case string(caps.KindSourceHut):
		return newSourceHut(name, p, auth.Secret, c, hc), nil
	case string(caps.KindTangled):
		return newTangled(name, p, auth.Secret, c, hc), nil
	case string(caps.KindGit):
		return newPlainGit(name, p, c), nil
	default:
		return nil, fmt.Errorf("unsupported platform kind %q", p.Kind)
	}
}

// JoinGitURL builds a clone/push URL from a base and repo coordinates.
func JoinGitURL(base, owner, name string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return owner + "/" + name + ".git"
	}
	base = strings.TrimSuffix(base, ".git")
	if strings.HasSuffix(base, "/"+owner) {
		return base + "/" + name + ".git"
	}
	return base + "/" + owner + "/" + name + ".git"
}

// SSHCommand returns a GIT_SSH_COMMAND for the configured SSH reference. A value
// containing a path separator or a known key filename is treated as a key path;
// otherwise ssh's own config supplies the identity.
func SSHCommand(ref config.AuthRef) string {
	v := ref.Value
	if strings.ContainsAny(v, `/\`) || strings.HasPrefix(v, "id_") || strings.HasSuffix(v, ".pem") {
		return fmt.Sprintf("ssh -i %q -o IdentitiesOnly=yes", v)
	}
	return "ssh -o IdentitiesOnly=yes"
}

// HTTPGitEnv sets an extra http header through git's environment-based config,
// keeping the token out of argv.
func HTTPGitEnv(gitURL, headerValue string) []string {
	if headerValue == "" {
		return nil
	}
	u, err := url.Parse(gitURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	base := u.Scheme + "://" + u.Host + "/"
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + base + ".extraheader",
		"GIT_CONFIG_VALUE_0=" + headerValue,
	}
}

func basicAuth(user, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

// HTTPError is a non-2xx response with a bounded body excerpt.
type HTTPError struct {
	Op     string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, body)
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// IsAuthError reports 401/403.
func IsAuthError(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden)
}

// IsConflict reports a duplicate-resource response.
func IsConflict(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == http.StatusConflict || he.Status == http.StatusUnprocessableEntity)
}

type apiClient struct {
	name    string
	base    string
	token   string
	headers map[string]string
	hc      *http.Client
}

func newAPI(name, base, token string, headers map[string]string, hc *http.Client) *apiClient {
	if hc == nil {
		hc = &http.Client{Timeout: 45 * time.Second}
	}
	return &apiClient{name: name, base: strings.TrimRight(base, "/"), token: token, headers: headers, hc: hc}
}

func (a *apiClient) url(path string) string {
	if path == "" {
		return a.base
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return a.base + path
}

func (a *apiClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.url(path), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nuncio/1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", a.name, method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Op: a.name + " " + method + " " + path, Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s: decode response: %w", a.name, err)
		}
	}
	return nil
}

// plainGit is a bare git endpoint with no API. It exists for local deployments
// and for tests that need a real remote without a forge account.
type plainGit struct {
	name string
	cfg  config.Platform
	caps caps.Capabilities
}

func newPlainGit(name string, p config.Platform, c caps.Capabilities) *plainGit {
	return &plainGit{name: name, cfg: p, caps: c}
}

func (p *plainGit) Name() string                 { return p.name }
func (p *plainGit) Kind() string                 { return string(caps.KindGit) }
func (p *plainGit) Caps() caps.Capabilities      { return p.caps }
func (p *plainGit) Close() error                 { return nil }
func (p *plainGit) Verify(context.Context) error { return nil }

func (p *plainGit) GitURL(owner, name string) string {
	return JoinGitURL(p.cfg.Git, owner, name)
}

func (p *plainGit) GitEnv() []string {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if p.cfg.Auth != "" {
		ref, err := config.ParseAuthRef(p.cfg.Auth)
		if err == nil && ref.Scheme == "ssh" {
			env = append(env, "GIT_SSH_COMMAND="+SSHCommand(ref))
		}
	}
	return env
}

func (p *plainGit) ListRepos(context.Context) ([]Repo, error) { return nil, nil }

func (p *plainGit) CreateRepo(context.Context, Repo) (Repo, error) {
	return Repo{}, ErrUnsupported
}

func (p *plainGit) GetRepo(_ context.Context, owner, name string) (Repo, error) {
	return Repo{Owner: owner, Name: name, Private: false}, nil
}

func (p *plainGit) SetMeta(context.Context, string, string, Meta) error { return ErrUnsupported }

func (p *plainGit) Unprotect(context.Context, string, string, string) error { return nil }
func (p *plainGit) Reprotect(context.Context, string, string, string) error {
	return nil
}

// WriteTempAskPass is used by tests to prove tokens never reach argv. It is not
// part of the runtime path.
func WriteTempAskPass(dir, content string) (string, error) {
	path := dir + string(os.PathSeparator) + "askpass.sh"
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		return "", err
	}
	return path, nil
}
