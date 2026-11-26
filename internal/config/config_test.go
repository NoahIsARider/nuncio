package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `
version = 1

[state]
path = "/var/lib/nuncio/state.db"
cache = "/var/lib/nuncio/repos"

[daemon]
listen = "127.0.0.1:8787"
interval = "15m"
concurrency = 4
dry_run = true

[identity]
name = "nuncio"
email = "nuncio@localhost"

[platforms.github]
kind = "github"
api  = "https://api.github.com"
git  = "https://github.com"
auth = "env:GITHUB_TOKEN"

[platforms.gitlab]
kind = "gitlab"
api  = "https://gitlab.com/api/v4"
git  = "ssh://git@gitlab.com"
auth = "env:GITLAB_TOKEN"

[platforms.codeberg]
kind = "forgejo"
api  = "https://codeberg.org/api/v1"
git  = "ssh://git@codeberg.org"
auth = "ssh:codeberg"

[platforms.srht]
kind = "sourcehut"
api  = "https://git.sr.ht/query"
git  = "ssh://git@git.sr.ht"
auth = "env:SRHT_TOKEN"
pre_receive_gate = true

[platforms.tangled]
kind     = "tangled"
knot     = "tangled.org"
owner    = "alice.tngl.sh"
git      = "ssh://git@tangled.org"
auth     = "ssh:tangled"
firehose = "wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos"

[policy.default]
platforms       = ["github", "codeberg"]
authority       = "none"
on_divergence   = "park"
on_delete       = "ignore"
snapshot        = true
verify          = true

[policy.site]
platforms       = ["github", "gitlab", "codeberg", "srht"]
default_branch  = "main"
authority       = "none"

[policy.paper]
platforms       = ["github", "gitlab"]
authority       = "github"
on_divergence   = "park"

[repos."alice/example"]
policy    = "default"
platforms = ["github", "codeberg", "tangled"]

[repos."alice/example".refs."refs/heads/main"]
authority = "github"

[repos."alice/example".refs."refs/tags/*"]
on_divergence = "park"
on_delete     = "propagate"

[repos."alice/site"]
policy    = "site"
platforms = ["github", "gitlab", "codeberg"]

[groups.papers]
policy = "paper"
select = { topic = "paper", platform = "github" }
`

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nuncio.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSample(t *testing.T) {
	c, err := Load(writeTempConfig(t, sampleConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Version != 1 {
		t.Errorf("version = %d", c.Version)
	}
	if got := len(c.Platforms); got != 5 {
		t.Errorf("platforms = %d, want 5", got)
	}
	if c.Platforms["tangled"].Knot != "tangled.org" {
		t.Errorf("tangled knot = %q", c.Platforms["tangled"].Knot)
	}
	if !c.Platforms["srht"].PreReceiveGate {
		t.Error("srht gate should be true")
	}
	pol, err := c.PolicyFor("alice/example")
	if err != nil {
		t.Fatal(err)
	}
	if pol.OnDivergence != DivergencePark || pol.OnDelete != DeleteIgnore || !pol.Snapshot || !pol.Verify {
		t.Errorf("defaults not applied: %+v", pol)
	}
	if got := strings.Join(pol.Platforms, ","); got != "github,codeberg,tangled" {
		t.Errorf("repo platforms = %s", got)
	}
	main := pol.ForRef("refs/heads/main")
	if main.Authority != "github" {
		t.Errorf("main authority = %q", main.Authority)
	}
	tags := pol.ForRef("refs/tags/v1.2.0")
	if tags.OnDelete != DeletePropagate {
		t.Errorf("tag on_delete = %q", tags.OnDelete)
	}
	if tags.Authority != "none" {
		t.Errorf("tag authority = %q, want none", tags.Authority)
	}
	site, err := c.PolicyFor("alice/site")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(site.Platforms, ","); got != "github,gitlab,codeberg" {
		t.Errorf("site platforms = %s", got)
	}
}

func TestRefRulePrecedence(t *testing.T) {
	body := `
version = 1
[platforms.github]
kind = "github"
git = "https://github.com"
auth = "env:GITHUB_TOKEN"
[policy.default]
platforms = ["github"]
[repos."a/b"]
platforms = ["github"]
[repos."a/b".refs."refs/*"]
authority = "none"
[repos."a/b".refs."refs/heads/*"]
on_delete = "propagate"
[repos."a/b".refs."refs/heads/main"]
authority = "github"
`
	c, err := Load(writeTempConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	pol, _ := c.PolicyFor("a/b")
	main := pol.ForRef("refs/heads/main")
	if main.Authority != "github" {
		t.Errorf("main authority = %q", main.Authority)
	}
	if main.OnDelete != DeleteIgnore {
		t.Errorf("main on_delete = %q: the most specific rule wins outright", main.OnDelete)
	}
	other := pol.ForRef("refs/heads/other")
	if other.OnDelete != DeletePropagate {
		t.Errorf("other on_delete = %q", other.OnDelete)
	}
	tag := pol.ForRef("refs/tags/v1")
	if tag.OnDelete != DeleteIgnore {
		t.Errorf("tag on_delete = %q, want repo default", tag.OnDelete)
	}
}

func TestValidationFailures(t *testing.T) {
	valid := `
version = 1
[platforms.github]
kind = "github"
git = "https://github.com"
auth = "env:GITHUB_TOKEN"
[policy.default]
platforms = ["github"]
[repos."a/b"]
platforms = ["github"]
`
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"snapshot-false", func(s string) string {
			return strings.Replace(s, "[repos.\"a/b\"]\nplatforms = [\"github\"]",
				"[repos.\"a/b\"]\nplatforms = [\"github\"]\nsnapshot = false", 1)
		}, "snapshot = false"},
		{"verify-false", func(s string) string {
			return strings.Replace(s, "[repos.\"a/b\"]\nplatforms = [\"github\"]",
				"[repos.\"a/b\"]\nplatforms = [\"github\"]\nverify = false", 1)
		}, "verify = false"},
		{"literal-token", func(s string) string {
			return strings.Replace(s, `auth = "env:GITHUB_TOKEN"`, `auth = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, 1)
		}, "literal secret"},
		{"force-without-authority", func(s string) string {
			return strings.Replace(s, "platforms = [\"github\"]", "platforms = [\"github\"]\non_divergence = \"force-from-authority\"", 1)
		}, "force-from-authority"},
		{"unknown-platform", func(s string) string {
			return strings.Replace(s, "[repos.\"a/b\"]\nplatforms = [\"github\"]",
				"[repos.\"a/b\"]\nplatforms = [\"nope\"]", 1)
		}, "unknown platform"},
		{"unknown-key", func(s string) string { return s + "\nbogus = 1\n" }, "unknown keys"},
		{"authority-not-member", func(s string) string {
			return strings.Replace(s, "[repos.\"a/b\"]\nplatforms = [\"github\"]",
				"[repos.\"a/b\"]\nplatforms = [\"github\"]\nauthority = \"gitlab\"", 1)
		}, "not one of the repo"},
		{"bad-glob", func(s string) string { return s + "\n[repos.\"a/b\".refs.\"heads/main\"]\n" }, "must start with refs/"},
		{"bad-group-selector", func(s string) string {
			return s + "\n[groups.x]\npolicy = \"default\"\nselect = { colour = \"red\" }\n"
		}, "not a supported selector"},
		{"gate-wrong-kind", func(s string) string {
			return strings.Replace(s, `auth = "env:GITHUB_TOKEN"`, "auth = \"env:GITHUB_TOKEN\"\npre_receive_gate = true", 1)
		}, "only meaningful"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tc.mutate(valid)))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	c, err := Load(writeTempConfig(t, sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nuncio.toml")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatalf("reload encoded config: %v", err)
	}
	pol1, _ := c.PolicyFor("alice/example")
	pol2, _ := again.PolicyFor("alice/example")
	if strings.Join(pol1.Platforms, ",") != strings.Join(pol2.Platforms, ",") {
		t.Errorf("platforms changed: %v vs %v", pol1.Platforms, pol2.Platforms)
	}
	if pol1.ForRef("refs/heads/main").Authority != pol2.ForRef("refs/heads/main").Authority {
		t.Error("ref authority changed after round trip")
	}
	if len(again.Platforms) != len(c.Platforms) {
		t.Errorf("platform count changed: %d vs %d", len(again.Platforms), len(c.Platforms))
	}
}

func TestParseRemotes(t *testing.T) {
	out := `origin	git@github.com:alice/example.git (fetch)
origin	git@github.com:alice/example.git (push)
codeberg	ssh://git@codeberg.org/alice/example.git (fetch)
srht	git@git.sr.ht:~alice/example (fetch)
tangled	ssh://git@tangled.org/alice.tngl.sh/example (fetch)
local	C:/srv/git/local.git (fetch)
`
	rs := ParseRemotes(out)
	if len(rs) != 5 {
		t.Fatalf("got %d remotes: %+v", len(rs), rs)
	}
	if rs[0].Platform != "github" || rs[0].Slug() != "alice/example" {
		t.Errorf("github = %+v", rs[0])
	}
	if rs[1].Platform != "codeberg" || rs[1].Slug() != "alice/example" {
		t.Errorf("codeberg = %+v", rs[1])
	}
	if rs[2].Platform != "srht" || rs[2].Slug() != "alice/example" {
		t.Errorf("srht = %+v", rs[2])
	}
	if rs[3].Platform != "tangled" || rs[3].Slug() != "alice.tngl.sh/example" {
		t.Errorf("tangled = %+v", rs[3])
	}
	if rs[4].Platform != "" {
		t.Errorf("local should be unrecognised: %+v", rs[4])
	}
}

func TestGenerateInit(t *testing.T) {
	rs := []Remote{
		{Name: "origin", Platform: "github", Owner: "alice", Repo: "example", Host: "github.com"},
		{Name: "codeberg", Platform: "codeberg", Owner: "alice", Repo: "example", Host: "codeberg.org"},
	}
	c, slug, err := GenerateInit(InitOptions{Remotes: rs})
	if err != nil {
		t.Fatal(err)
	}
	if slug != "alice/example" {
		t.Errorf("slug = %q", slug)
	}
	if !c.Daemon.DryRun {
		t.Error("init must write dry_run = true")
	}
	pol, _ := c.PolicyFor(slug)
	if pol.OnDivergence != DivergencePark || pol.Authority != AuthorityNone {
		t.Errorf("unsafe policy: %+v", pol)
	}
	if len(pol.Platforms) != 2 {
		t.Errorf("platforms = %v", pol.Platforms)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nuncio.toml")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Daemon.DryRun {
		t.Error("encoded config lost dry_run")
	}
	if err := UpdateDryRun(path, false); err != nil {
		t.Fatal(err)
	}
	flipped, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if flipped.Daemon.DryRun {
		t.Error("UpdateDryRun did not flip the value")
	}
}

func TestGenerateInitOneForgeIsInformative(t *testing.T) {
	rs := []Remote{{Name: "origin", Platform: "github", Owner: "a", Repo: "b", Host: "github.com"}}
	c, _, err := GenerateInit(InitOptions{Remotes: rs})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Platforms) != 1 {
		t.Fatalf("expected one platform, got %v", c.Platforms)
	}
}

func TestAuthRefs(t *testing.T) {
	if err := ValidateAuthRef("env:GITHUB_TOKEN"); err != nil {
		t.Errorf("env ref rejected: %v", err)
	}
	if err := ValidateAuthRef("file:/run/secrets/token"); err != nil {
		t.Errorf("file ref rejected: %v", err)
	}
	if err := ValidateAuthRef("ssh:codeberg"); err != nil {
		t.Errorf("ssh ref rejected: %v", err)
	}
	if err := ValidateAuthRef("ghp_0123456789abcdefghijklmnopqrstuvwxyz"); err == nil {
		t.Error("literal token accepted")
	}
	got, err := ResolveWith("env:TOKEN", func(k string) string {
		if k == "TOKEN" {
			return "s3cret"
		}
		return ""
	}, func(string) ([]byte, error) { return nil, os.ErrNotExist })
	if err != nil || got != "s3cret" {
		t.Errorf("resolve env = %q, %v", got, err)
	}
	if _, err := ResolveWith("ssh:codeberg", nil, nil); err == nil {
		t.Error("ssh should not resolve to a bearer token")
	}
	if err := ValidateAuthRef("literal-value"); err == nil {
		t.Error("unprefixed auth accepted")
	}
}
