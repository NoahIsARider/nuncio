// Command nuncio is a courier between sovereign forges. It keeps one
// repository in sync across GitHub, GitLab, Forgejo/Codeberg, SourceHut and
// Tangled without electing a hub.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/NoahIsARider/nuncio/internal/app"
	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/execute"
)

// Set with -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Contractual exit codes.
const (
	exitOK          = 0
	exitFindings    = 1
	exitOperational = 2
	exitSafety      = 3
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	configPath, rest, err := extractConfigFlag(args)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio:", err)
		usage(stderr)
		return exitOperational
	}
	if configPath == "" {
		configPath = resolveConfigPath("")
	}
	if len(rest) == 0 {
		usage(stderr)
		return exitOperational
	}
	command, tail := rest[0], rest[1:]
	switch command {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "version":
		return cmdVersion(tail, stdout, stderr)
	case "init":
		return cmdInit(tail, configPath, stdout, stderr)
	case "doctor":
		return cmdDoctor(tail, configPath, stdout, stderr)
	case "discover":
		return cmdDiscover(tail, configPath, stdout, stderr)
	case "plan":
		return cmdPlan(tail, configPath, stdout, stderr)
	case "sync":
		return cmdSync(tail, configPath, stdout, stderr)
	case "serve":
		return cmdServe(tail, configPath, stdout, stderr)
	case "report":
		return cmdReport(tail, configPath, stdout, stderr)
	case "lost":
		return cmdLost(tail, configPath, stdout, stderr)
	case "snapshot":
		return cmdSnapshot(tail, configPath, stdout, stderr)
	case "config":
		return cmdConfig(tail, configPath, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "nuncio: unknown command %q\n\n", command)
		usage(stderr)
		return exitOperational
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `nuncio — a courier between sovereign forges

usage:
  nuncio [-c nuncio.toml] <command> [flags]

commands:
  init [--forge github,codeberg] [--force] [--print]
  doctor [--print-systemd] [--print-docker] [--json]
  discover [--platform P] [--write] [--policy NAME]
  plan [--repo R] [--json]
  sync [--repo R] [--dry-run] [--json]
  serve [--listen ADDR]
  report [--repo R] [--json] [--since DUR]
  lost [--repo R] [--json]
  snapshot list|create|restore [flags]
  config set daemon.dry_run true|false
  config get daemon.dry_run
  version

exit codes: 0 clean, 1 findings for you, 2 operational error, 3 safety refusal.
`)
}

// extractConfigFlag lets -c/--config appear anywhere, which matters for
// `nuncio config set ... -c path` style invocations.
func extractConfigFlag(args []string) (string, []string, error) {
	var path string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s needs a path", a)
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			path = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-c="):
			path = strings.TrimPrefix(a, "-c=")
		default:
			rest = append(rest, a)
		}
	}
	return path, rest, nil
}

func resolveConfigPath(path string) string {
	if path != "" {
		return path
	}
	if env := os.Getenv("NUNCIO_CONFIG"); env != "" {
		return env
	}
	return "nuncio.toml"
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s does not exist; run `nuncio init` first", path)
		}
		return nil, err
	}
	return cfg, nil
}

func newApp(ctx context.Context, path string, stderr io.Writer) (*app.App, *config.Config, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	a, err := app.Open(ctx, cfg, app.Options{
		Logf: func(format string, args ...any) {
			fmt.Fprintf(stderr, "nuncio: "+format+"\n", args...)
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return a, cfg, nil
}

func selectSlugs(ctx context.Context, a *app.App, repoFlag string) ([]string, []string, error) {
	if repoFlag != "" {
		slugs := splitList(repoFlag)
		for _, slug := range slugs {
			if _, err := a.Spec(ctx, slug); err != nil {
				return nil, nil, err
			}
		}
		return slugs, nil, nil
	}
	specs, notices, err := a.RepoSpecs(ctx)
	if err != nil {
		return nil, nil, err
	}
	slugs := make([]string, 0, len(specs))
	for _, spec := range specs {
		slugs = append(slugs, spec.Slug)
	}
	return slugs, notices, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func renderRuns(runs []*app.Run, asJSON bool, w io.Writer) error {
	if !asJSON {
		for i, run := range runs {
			if i > 0 {
				fmt.Fprintln(w)
			}
			if err := run.Data().RenderHuman(w); err != nil {
				return err
			}
		}
		return nil
	}
	if len(runs) == 1 {
		return runs[0].Data().RenderJSON(w)
	}
	raws := make([]json.RawMessage, 0, len(runs))
	for _, run := range runs {
		var buf bytes.Buffer
		if err := run.Data().RenderJSON(&buf); err != nil {
			return err
		}
		raws = append(raws, buf.Bytes())
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(raws)
}

func classify(err error) int {
	if err == nil {
		return exitOK
	}
	if execute.IsSafety(err) {
		return exitSafety
	}
	return exitOperational
}

func higher(a, b int) int {
	if b > a {
		return b
	}
	return a
}

func runOutcome(run *app.Run, err error) int {
	if err != nil {
		return classify(err)
	}
	if run.Operational() {
		return exitOperational
	}
	if run.Findings() > 0 {
		return exitFindings
	}
	return exitOK
}

// newFlagSet returns a quiet flag set with a consistent error prefix.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) bool {
	if err := fs.Parse(args); err != nil {
		return false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return false
	}
	return true
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version", stderr)
	if !parseFlags(fs, args) {
		return exitOperational
	}
	fmt.Fprintf(stdout, "nuncio %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
	return exitOK
}

func parseSince(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	return time.ParseDuration(value)
}
