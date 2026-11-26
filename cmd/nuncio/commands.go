package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NoahIsARider/nuncio/internal/app"
	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/report"
)

func cmdInit(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("init", stderr)
	forge := fs.String("forge", "", "additional forges to scaffold, comma separated")
	force := fs.Bool("force", false, "overwrite an existing config")
	printOnly := fs.Bool("print", false, "print the config instead of writing it")
	dir := fs.String("dir", "", "directory whose git remotes are detected (default: cwd)")
	interval := fs.String("interval", "15m", "reconcile interval")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	where := *dir
	if where == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "nuncio init:", err)
			return exitOperational
		}
		where = wd
	}
	cmd := exec.Command("git", "remote", "-v")
	cmd.Dir = where
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(stderr, "nuncio init: cannot read git remotes in %s: %v\n", where, err)
		return exitOperational
	}
	remotes := config.ParseRemotes(string(out))
	if len(remotes) == 0 {
		fmt.Fprintln(stderr, "nuncio init: no git remotes found; add one, or pass --forge to scaffold a target")
		return exitFindings
	}
	cfg, slug, err := config.GenerateInit(config.InitOptions{
		Remotes: remotes, Forge: splitList(*forge), Interval: *interval,
	})
	if err != nil {
		fmt.Fprintln(stderr, "nuncio init:", err)
		return exitFindings
	}
	if *printOnly {
		fmt.Fprint(stdout, cfg.Encode())
		return exitOK
	}
	if _, err := os.Stat(configPath); err == nil && !*force {
		fmt.Fprintf(stderr, "nuncio init: %s already exists; pass --force to overwrite\n", configPath)
		return exitFindings
	}
	if err := config.Save(configPath, cfg); err != nil {
		fmt.Fprintln(stderr, "nuncio init:", err)
		return exitOperational
	}

	names := cfg.PlatformNames()
	fmt.Fprintf(stdout, "wrote %s\n", configPath)
	fmt.Fprintf(stdout, "repo   %s\n", slug)
	fmt.Fprintf(stdout, "forges %s\n\n", strings.Join(names, ", "))
	fmt.Fprintln(stdout, "dry_run = true. Nothing will be pushed until you turn it off with:")
	fmt.Fprintln(stdout, "  nuncio config set daemon.dry_run false")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "next:")
	fmt.Fprintln(stdout, "  nuncio doctor    # names missing tokens, never their values")
	fmt.Fprintln(stdout, "  nuncio plan      # what would happen; changes nothing")
	fmt.Fprintln(stdout, "  nuncio serve     # or `nuncio sync` for one shot")
	if len(names) == 1 {
		fmt.Fprintf(stdout, "\nonly one forge was detected. Add another with:\n  nuncio init --forge %s\n",
			suggestForge(names[0]))
	}
	return exitOK
}

func suggestForge(existing string) string {
	for _, candidate := range []string{"github", "gitlab", "codeberg", "srht", "tangled"} {
		if candidate != existing {
			return candidate
		}
	}
	return "codeberg"
}

const systemdUnit = `[Unit]
Description=nuncio — multi-forge git courier
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nuncio
Group=nuncio
EnvironmentFile=/etc/nuncio/env
ExecStart=/usr/local/bin/nuncio -c /etc/nuncio/nuncio.toml serve
Restart=always
RestartSec=5
StateDirectory=nuncio
WorkingDirectory=/var/lib/nuncio
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/nuncio

[Install]
WantedBy=multi-user.target
`

const dockerSnippet = `# docker-compose.yml — timer-only shape; add ports only for webhooks.
services:
  nuncio:
    image: ghcr.io/noahisarider/nuncio:latest
    command: ["-c", "/etc/nuncio/nuncio.toml", "serve"]
    volumes:
      - ./nuncio.toml:/etc/nuncio/nuncio.toml:ro
      - nuncio-state:/var/lib/nuncio
      - ${HOME}/.ssh:/home/nuncio/.ssh:ro
    environment:
      - GITHUB_TOKEN
      - GITLAB_TOKEN
      - SRHT_TOKEN
    restart: unless-stopped
    # ports:
    #   - "127.0.0.1:8787:8787"

volumes:
  nuncio-state:
`

func cmdDoctor(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	printSystemd := fs.Bool("print-systemd", false, "print a systemd unit and exit")
	printDocker := fs.Bool("print-docker", false, "print a compose service and exit")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	if *printSystemd {
		fmt.Fprint(stdout, systemdUnit)
		return exitOK
	}
	if *printDocker {
		fmt.Fprint(stdout, dockerSnippet)
		return exitOK
	}
	ctx := context.Background()
	a, cfg, err := newApp(ctx, configPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio doctor:", err)
		return exitOperational
	}
	defer a.Close()
	rows := a.Doctor(ctx)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"config": configPath, "dry_run": cfg.Daemon.DryRun,
			"platforms": rows, "warnings": a.Warnings(),
		}); err != nil {
			fmt.Fprintln(stderr, "nuncio doctor:", err)
			return exitOperational
		}
		return exitOK
	}
	fmt.Fprint(stdout, app.FormatDoctor(rows))
	for _, w := range a.Warnings() {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	fmt.Fprintf(stdout, "\ndry_run = %t (turn off with: nuncio config set daemon.dry_run false)\n", cfg.Daemon.DryRun)
	return exitOK
}

func cmdDiscover(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("discover", stderr)
	platform := fs.String("platform", "", "only list this platform")
	write := fs.Bool("write", false, "append missing [repos.*] blocks to the config")
	policy := fs.String("policy", "default", "policy to assign discovered repositories")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	ctx := context.Background()
	a, cfg, err := newApp(ctx, configPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio discover:", err)
		return exitOperational
	}
	defer a.Close()
	found, err := a.Discover(ctx, *platform)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio discover:", err)
		return exitOperational
	}
	platforms := policyPlatforms(cfg, *policy)
	var slugs []string
	for _, name := range sortedMapKeys(found) {
		repos := found[name]
		fmt.Fprintf(stdout, "# %s (%d)\n", name, len(repos))
		for _, repo := range repos {
			slug := repo.Owner + "/" + repo.Name
			slugs = append(slugs, slug)
			fmt.Fprint(stdout, config.AddRepoBlock(slug, *policy, platforms))
			fmt.Fprintln(stdout)
		}
	}
	slugs = uniqueStrings(slugs)
	if len(slugs) == 0 {
		fmt.Fprintln(stdout, "(no repositories)")
		return exitOK
	}
	if *write {
		if err := appendRepoBlocks(cfg, configPath, slugs, *policy, platforms); err != nil {
			fmt.Fprintln(stderr, "nuncio discover:", err)
			return exitOperational
		}
		fmt.Fprintf(stdout, "# wrote %d repo block(s) to %s\n", len(slugs), configPath)
	}
	return exitOK
}

func appendRepoBlocks(cfg *config.Config, path string, slugs []string, policy string, platforms []string) error {
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var add strings.Builder
	for _, slug := range slugs {
		if _, exists := cfg.Repos[slug]; exists {
			continue
		}
		add.WriteString(config.AddRepoBlock(slug, policy, platforms))
	}
	if add.Len() == 0 {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(original, []byte(add.String())...), 0o644); err != nil {
		return err
	}
	if _, err := config.Load(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("discovered block would make the config invalid: %w", err)
	}
	return os.Rename(tmp, path)
}

func cmdPlan(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plan", stderr)
	repo := fs.String("repo", "", "only this repository (owner/name)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	ctx := context.Background()
	a, _, err := newApp(ctx, configPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio plan:", err)
		return exitOperational
	}
	defer a.Close()
	slugs, notices, err := selectSlugs(ctx, a, *repo)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio plan:", err)
		return exitOperational
	}
	for _, notice := range notices {
		fmt.Fprintln(stderr, "notice:", notice)
	}
	if len(slugs) == 0 {
		fmt.Fprintln(stderr, "nuncio plan: no repositories are configured")
		return exitOK
	}
	var runs []*app.Run
	code := exitOK
	if len(notices) > 0 {
		code = exitFindings
	}
	for _, slug := range slugs {
		run, err := a.Plan(ctx, slug)
		if err != nil {
			fmt.Fprintf(stderr, "nuncio plan: %s: %v\n", slug, err)
			code = higher(code, exitOperational)
			continue
		}
		runs = append(runs, run)
		code = higher(code, runOutcome(run, nil))
	}
	if err := renderRuns(runs, *asJSON, stdout); err != nil {
		fmt.Fprintln(stderr, "nuncio plan:", err)
		return exitOperational
	}
	return code
}

func cmdSync(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("sync", stderr)
	repo := fs.String("repo", "", "only this repository (owner/name)")
	dryRun := fs.Bool("dry-run", false, "plan and verify but mutate nothing")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	ctx := context.Background()
	a, cfg, err := newApp(ctx, configPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio sync:", err)
		return exitOperational
	}
	defer a.Close()
	slugs, notices, err := selectSlugs(ctx, a, *repo)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio sync:", err)
		return exitOperational
	}
	for _, notice := range notices {
		fmt.Fprintln(stderr, "notice:", notice)
	}
	if len(slugs) == 0 {
		fmt.Fprintln(stderr, "nuncio sync: no repositories are configured")
		return exitOK
	}
	effectiveDry := *dryRun || cfg.Daemon.DryRun
	if cfg.Daemon.DryRun && !*dryRun {
		fmt.Fprintln(stderr, "nuncio sync: daemon.dry_run = true in the config; mutating nothing. Turn it off with `nuncio config set daemon.dry_run false`.")
	}
	var runs []*app.Run
	code := exitOK
	if len(notices) > 0 {
		code = exitFindings
	}
	for _, slug := range slugs {
		run, err := a.Sync(ctx, slug, effectiveDry)
		if err != nil {
			fmt.Fprintf(stderr, "nuncio sync: %s: %v\n", slug, err)
			code = higher(code, classify(err))
		}
		if run != nil {
			runs = append(runs, run)
			code = higher(code, runOutcome(run, nil))
		}
	}
	if err := renderRuns(runs, *asJSON, stdout); err != nil {
		fmt.Fprintln(stderr, "nuncio sync:", err)
		return exitOperational
	}
	return code
}

func cmdReport(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("report", stderr)
	repo := fs.String("repo", "", "only this repository (owner/name)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	since := fs.String("since", "", "list stored runs from this duration (e.g. 24h)")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	duration, err := parseSince(*since)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio report: --since:", err)
		return exitOperational
	}
	ctx := context.Background()
	a, _, err := newApp(ctx, configPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio report:", err)
		return exitOperational
	}
	defer a.Close()
	slugs, notices, err := selectSlugs(ctx, a, *repo)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio report:", err)
		return exitOperational
	}
	for _, notice := range notices {
		fmt.Fprintln(stderr, "notice:", notice)
	}
	if len(slugs) == 0 {
		fmt.Fprintln(stderr, "nuncio report: no repositories are configured")
		return exitOK
	}
	if duration > 0 {
		return reportSince(ctx, a, slugs, time.Now().Add(-duration).Unix(), *asJSON, stdout, stderr)
	}

	var runs []*app.Run
	code := exitOK
	if len(notices) > 0 {
		code = exitFindings
	}
	for _, slug := range slugs {
		run, err := a.Plan(ctx, slug)
		if err != nil {
			fmt.Fprintf(stderr, "nuncio report: %s: %v\n", slug, err)
			code = higher(code, exitOperational)
			continue
		}
		runs = append(runs, run)
		code = higher(code, runOutcome(run, nil))
	}
	if err := renderRuns(runs, *asJSON, stdout); err != nil {
		fmt.Fprintln(stderr, "nuncio report:", err)
		return exitOperational
	}
	return code
}

type storedReport struct {
	Repo       string          `json:"repo"`
	ID         int64           `json:"id"`
	StartedAt  int64           `json:"started_at"`
	FinishedAt int64           `json:"finished_at"`
	DryRun     bool            `json:"dry_run"`
	Lost       int64           `json:"lost_commits"`
	Report     json.RawMessage `json:"report,omitempty"`
}

func reportSince(ctx context.Context, a *app.App, slugs []string, cutoff int64, asJSON bool, stdout, stderr io.Writer) int {
	var rows []storedReport
	for _, slug := range slugs {
		records, err := a.ReportsSince(ctx, slug, cutoff)
		if err != nil {
			fmt.Fprintf(stderr, "nuncio report: %s: %v\n", slug, err)
			return exitOperational
		}
		for _, rec := range records {
			row := storedReport{
				Repo: slug, ID: rec.ID, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt,
				DryRun: rec.DryRun, Lost: rec.Lost,
			}
			if strings.TrimSpace(rec.JSON) != "" {
				row.Report = json.RawMessage(rec.JSON)
			}
			rows = append(rows, row)
		}
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintln(stderr, "nuncio report:", err)
			return exitOperational
		}
		return exitOK
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no stored report runs in that window")
		return exitOK
	}
	fmt.Fprintf(stdout, "%-28s  %-19s  %-7s  %-8s\n", "REPO", "FINISHED", "DRY-RUN", "LOST")
	for _, row := range rows {
		lost := strconv.FormatInt(row.Lost, 10)
		if row.Lost < 0 {
			lost = "unknown"
		}
		fmt.Fprintf(stdout, "%-28s  %-19s  %-7t  %-8s\n",
			row.Repo, time.Unix(row.FinishedAt, 0).Format("2006-01-02 15:04:05"), row.DryRun, lost)
	}
	return exitOK
}

func cmdLost(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("lost", stderr)
	repo := fs.String("repo", "", "only this repository (owner/name)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	ctx := context.Background()
	a, _, err := newApp(ctx, configPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio lost:", err)
		return exitOperational
	}
	defer a.Close()
	slugs, notices, err := selectSlugs(ctx, a, *repo)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio lost:", err)
		return exitOperational
	}
	for _, notice := range notices {
		fmt.Fprintln(stderr, "notice:", notice)
	}
	type lostRow struct {
		Repo      string                `json:"repo"`
		Lost      report.Lost           `json:"lost"`
		Platforms []report.PlatformInfo `json:"platforms"`
		Unknown   map[string]string     `json:"unknown,omitempty"`
	}
	var rows []lostRow
	code := exitOK
	for _, slug := range slugs {
		lost, info, unknown, err := a.Lost(ctx, slug)
		if err != nil {
			fmt.Fprintf(stderr, "nuncio lost: %s: %v\n", slug, err)
			code = higher(code, exitOperational)
			continue
		}
		rows = append(rows, lostRow{Repo: slug, Lost: lost, Platforms: info, Unknown: unknown})
		if len(unknown) > 0 || !lost.Complete {
			code = higher(code, exitOperational)
		} else if lost.Count > 0 {
			code = higher(code, exitFindings)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintln(stderr, "nuncio lost:", err)
			return exitOperational
		}
		return code
	}
	for i, row := range rows {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "%s\n", row.Repo)
		var coords []string
		for _, p := range row.Platforms {
			coords = append(coords, p.Name+":"+p.Coordinates)
		}
		sort.Strings(coords)
		fmt.Fprintf(stdout, "  %s\n", strings.Join(coords, "   "))
		fmt.Fprintf(stdout, "  lost commits  %s\n", lostText(row.Lost))
		for platform, msg := range row.Unknown {
			fmt.Fprintf(stdout, "  unreachable   %s: %s\n", platform, msg)
		}
	}
	return code
}

func lostText(l report.Lost) string {
	if !l.Complete {
		if len(l.Missing) == 0 {
			return "unknown (objects unavailable; refusing to claim 0)"
		}
		shown := l.Missing
		if len(shown) > 3 {
			shown = shown[:3]
		}
		return fmt.Sprintf("unknown (%d objects unavailable: %s)", len(l.Missing), strings.Join(shown, ", "))
	}
	return strconv.Itoa(l.Count)
}

func cmdSnapshot(args []string, configPath string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: nuncio snapshot list|create|restore [flags]")
		return exitOperational
	}
	sub, tail := args[0], args[1:]
	ctx := context.Background()
	switch sub {
	case "list":
		fs := newFlagSet("snapshot list", stderr)
		repo := fs.String("repo", "", "repository (owner/name)")
		asJSON := fs.Bool("json", false, "machine-readable output")
		if !parseFlags(fs, tail) {
			return exitOperational
		}
		a, _, err := newApp(ctx, configPath, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "nuncio snapshot:", err)
			return exitOperational
		}
		defer a.Close()
		if *repo == "" {
			fmt.Fprintln(stderr, "nuncio snapshot list: --repo is required")
			return exitOperational
		}
		snaps, err := a.SnapshotList(ctx, *repo)
		if err != nil {
			fmt.Fprintln(stderr, "nuncio snapshot list:", err)
			return exitOperational
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(snaps); err != nil {
				fmt.Fprintln(stderr, "nuncio snapshot list:", err)
				return exitOperational
			}
			return exitOK
		}
		if len(snaps) == 0 {
			fmt.Fprintln(stdout, "(no snapshots)")
			return exitOK
		}
		fmt.Fprintf(stdout, "%-52s  %-10s  %-24s  %s\n", "TAG", "PLATFORM", "REF", "SHA")
		for _, s := range snaps {
			fmt.Fprintf(stdout, "%-52s  %-10s  %-24s  %s\n", s.Tag, s.Platform, s.Ref, s.SHA)
		}
		return exitOK

	case "create":
		fs := newFlagSet("snapshot create", stderr)
		repo := fs.String("repo", "", "repository (owner/name)")
		ref := fs.String("ref", "", "ref to snapshot (default: policy default branch)")
		platform := fs.String("platform", "", "platform to write the snapshot to")
		if !parseFlags(fs, tail) {
			return exitOperational
		}
		a, _, err := newApp(ctx, configPath, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "nuncio snapshot:", err)
			return exitOperational
		}
		defer a.Close()
		if *repo == "" {
			fmt.Fprintln(stderr, "nuncio snapshot create: --repo is required")
			return exitOperational
		}
		snap, err := a.SnapshotCreate(ctx, *repo, *ref, *platform)
		if err != nil {
			fmt.Fprintln(stderr, "nuncio snapshot create:", err)
			return classify(err)
		}
		fmt.Fprintf(stdout, "%s  %s  (%s %s)\n", snap.Tag, snap.SHA, snap.Platform, snap.Ref)
		return exitOK

	case "restore":
		fs := newFlagSet("snapshot restore", stderr)
		repo := fs.String("repo", "", "repository (owner/name)")
		name := fs.String("name", "", "snapshot tag name")
		ref := fs.String("ref", "", "ref to restore (default: the snapshot's ref)")
		platform := fs.String("platform", "", "platform to restore on (default: the snapshot's platform)")
		if !parseFlags(fs, tail) {
			return exitOperational
		}
		a, _, err := newApp(ctx, configPath, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "nuncio snapshot:", err)
			return exitOperational
		}
		defer a.Close()
		if *repo == "" || *name == "" {
			fmt.Fprintln(stderr, "nuncio snapshot restore: --repo and --name are required")
			return exitOperational
		}
		if err := a.SnapshotRestore(ctx, *repo, *name, *ref, *platform); err != nil {
			fmt.Fprintln(stderr, "nuncio snapshot restore:", err)
			return classify(err)
		}
		fmt.Fprintf(stdout, "restored %s from %s\n", *name, *repo)
		return exitOK
	default:
		fmt.Fprintf(stderr, "nuncio snapshot: unknown subcommand %q\n", sub)
		return exitOperational
	}
}

func cmdConfig(args []string, configPath string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: nuncio config set daemon.dry_run true|false | get daemon.dry_run")
		return exitOperational
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: nuncio config set daemon.dry_run true|false")
			return exitOperational
		}
		if args[1] != "daemon.dry_run" {
			fmt.Fprintf(stderr, "nuncio config set: unsupported key %q (only daemon.dry_run)\n", args[1])
			return exitOperational
		}
		value, err := strconv.ParseBool(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "nuncio config set: %q is not a boolean\n", args[2])
			return exitOperational
		}
		if err := config.UpdateDryRun(configPath, value); err != nil {
			fmt.Fprintln(stderr, "nuncio config set:", err)
			return exitOperational
		}
		fmt.Fprintf(stdout, "daemon.dry_run = %t in %s\n", value, configPath)
		return exitOK
	case "get":
		if len(args) != 2 || args[1] != "daemon.dry_run" {
			fmt.Fprintln(stderr, "usage: nuncio config get daemon.dry_run")
			return exitOperational
		}
		cfg, err := loadConfig(configPath)
		if err != nil {
			fmt.Fprintln(stderr, "nuncio config get:", err)
			return exitOperational
		}
		fmt.Fprintf(stdout, "%t\n", cfg.Daemon.DryRun)
		return exitOK
	default:
		fmt.Fprintf(stderr, "nuncio config: unknown subcommand %q\n", args[0])
		return exitOperational
	}
}

func policyPlatforms(cfg *config.Config, name string) []string {
	if pol, ok := cfg.Policy[name]; ok && pol.Platforms != nil && len(*pol.Platforms) > 0 {
		return *pol.Platforms
	}
	if pol, ok := cfg.Policy["default"]; ok && pol.Platforms != nil && len(*pol.Platforms) > 0 {
		return *pol.Platforms
	}
	return cfg.PlatformNames()
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
