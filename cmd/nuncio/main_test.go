package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/gitx"
)

func TestExamplesValidate(t *testing.T) {
	for _, name := range []string{"nuncio-2.toml", "nuncio-3.toml", "nuncio-5.toml"} {
		path := filepath.Join("..", "..", "examples", name)
		if _, err := config.Load(path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "nuncio") {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestInitConfigSetAndDoctorPrint(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "work")
	mustGit(t, dir, "init", "-b", "main", repoDir)
	mustGit(t, repoDir, "remote", "add", "origin", "git@github.com:alice/example.git")
	cfgPath := filepath.Join(dir, "nuncio.toml")

	var out, errOut bytes.Buffer
	if code := run([]string{"init", "--print", "--dir", repoDir, "-c", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("init --print: code = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "dry_run = true") || !strings.Contains(out.String(), "[platforms.github]") {
		t.Fatalf("init --print output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"init", "--dir", repoDir, "-c", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("init: code = %d, stderr = %s", code, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `[repos."alice/example"]`) {
		t.Fatalf("config does not contain the detected repo:\n%s", data)
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"config", "set", "daemon.dry_run", "false", "-c", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("config set: code = %d, stderr = %s", code, errOut.String())
	}
	out.Reset()
	if code := run([]string{"config", "get", "daemon.dry_run", "-c", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("config get: code = %d", code)
	}
	if strings.TrimSpace(out.String()) != "false" {
		t.Fatalf("config get = %q", out.String())
	}

	out.Reset()
	if code := run([]string{"doctor", "--print-systemd", "-c", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("doctor --print-systemd: code = %d", code)
	}
	if !strings.Contains(out.String(), "ExecStart=/usr/local/bin/nuncio") {
		t.Fatalf("systemd unit:\n%s", out.String())
	}
}

func TestPlanSyncReportLostCLI(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { makeWritable(root) })
	bareA := filepath.Join(root, "a", "alice", "example.git")
	bareB := filepath.Join(root, "b", "alice", "example.git")
	for _, bare := range []string{bareA, bareB} {
		if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
			t.Fatal(err)
		}
		mustGit(t, "", "init", "--bare", "--quiet", bare)
	}
	work := filepath.Join(root, "work")
	mustGit(t, "", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "commit", "-m", "one")
	mustGit(t, work, "push", gitx.FileURL(bareA), "main")

	cfgPath := filepath.Join(root, "nuncio.toml")
	toml := fmt.Sprintf(`version = 1
[state]
path = %q
cache = %q
[daemon]
listen = ""
interval = "1h"
concurrency = 2
dry_run = false
[identity]
name = "nuncio"
email = "nuncio@localhost"
[platforms.a]
kind = "git"
git = %q
[platforms.b]
kind = "git"
git = %q
[policy.default]
platforms = ["a", "b"]
authority = "none"
on_divergence = "park"
on_delete = "ignore"
snapshot = true
verify = true
[repos."alice/example"]
policy = "default"
`,
		filepath.ToSlash(filepath.Join(root, "state.db")),
		filepath.ToSlash(filepath.Join(root, "repos")),
		gitx.FileURL(filepath.Join(root, "a")),
		gitx.FileURL(filepath.Join(root, "b")),
	)
	if err := os.WriteFile(cfgPath, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"plan", "-c", cfgPath, "--json"}, &out, &errOut); code != exitFindings {
		t.Fatalf("plan code = %d, want 1 (created ref)\nstderr: %s\nstdout: %s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), `"class": "created"`) {
		t.Fatalf("plan output does not show a created ref:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"sync", "-c", cfgPath, "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("sync code = %d\nstderr: %s\nstdout: %s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), `"lost_commits": 0`) {
		t.Fatalf("sync report does not prove zero lost:\n%s", out.String())
	}
	if got := strings.TrimSpace(mustGit(t, "", "--git-dir", bareB, "rev-parse", "refs/heads/main")); got == "" {
		t.Fatal("sync did not push to b")
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"report", "-c", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("report code = %d\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "lost commits") || !strings.Contains(out.String(), "0") {
		t.Fatalf("report output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"lost", "-c", cfgPath}, &out, &errOut); code != exitOK {
		t.Fatalf("lost code = %d\nstderr: %s\nstdout: %s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "lost commits  0") {
		t.Fatalf("lost output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"report", "-c", cfgPath, "--since", "24h"}, &out, &errOut); code != exitOK {
		t.Fatalf("report --since code = %d", code)
	}
	if !strings.Contains(out.String(), "REPO") {
		t.Fatalf("report --since output:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"doctor", "-c", cfgPath, "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("doctor code = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"platforms"`) {
		t.Fatalf("doctor JSON:\n%s", out.String())
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func makeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			_ = os.Chmod(path, 0o666)
		}
		return nil
	})
}
