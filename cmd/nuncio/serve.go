package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/NoahIsARider/nuncio/internal/app"
	"github.com/NoahIsARider/nuncio/internal/config"
	"github.com/NoahIsARider/nuncio/internal/events"
	"github.com/NoahIsARider/nuncio/internal/state"
)

func cmdServe(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	listen := fs.String("listen", "", "override daemon.listen (empty disables the webhook listener)")
	if !parseFlags(fs, args) {
		return exitOperational
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(stderr, "%s nuncio: %s\n", time.Now().Format("2006-01-02T15:04:05"), fmt.Sprintf(format, args...))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, cfg, err := newApp(ctx, configPath, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio serve:", err)
		return exitOperational
	}
	defer a.Close()
	for _, warning := range a.Warnings() {
		logf("warning: %s", warning)
	}

	interval, err := time.ParseDuration(cfg.Daemon.Interval)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio serve:", err)
		return exitOperational
	}
	scheduler := events.NewScheduler(cfg.Daemon.Concurrency, interval, func(ctx context.Context, slug string) error {
		run, err := a.Sync(ctx, slug, cfg.Daemon.DryRun)
		if err != nil {
			logf("sync %s failed: %v", slug, err)
			return err
		}
		logf("sync %s: findings=%d dry_run=%t unreachable=%d lost=%s",
			slug, run.Findings(), run.DryRun, len(run.Unknown), lostSummary(run))
		return nil
	})
	scheduler.Logf = logf
	scheduler.Start(ctx)
	defer scheduler.Stop()

	specs, notices, err := a.RepoSpecs(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio serve:", err)
		return exitOperational
	}
	for _, notice := range notices {
		logf("notice: %s", notice)
	}
	if err := registerCoordinates(ctx, a, specs); err != nil {
		fmt.Fprintln(stderr, "nuncio serve:", err)
		return exitOperational
	}
	for _, spec := range specs {
		scheduler.Register(spec.Slug)
		scheduler.Wake(spec.Slug, "startup")
	}
	if pruned, err := a.Store.PruneJournal(ctx, time.Now().Add(-30*24*time.Hour).Unix()); err == nil && pruned > 0 {
		logf("pruned %d finished journal rows", pruned)
	}

	// Tangled is the one forge with a push stream rather than a webhook.
	for _, name := range cfg.PlatformNames() {
		p := cfg.Platforms[name]
		if p.Kind != "tangled" || p.Firehose == "" {
			continue
		}
		dids := firehoseDIDs(ctx, a, name, specs, logf)
		if len(dids) == 0 {
			logf("firehose %s: no repositories advertised a DID; falling back to the timer", name)
			continue
		}
		firehose := events.NewFirehose(p.Firehose, dids, func(slug string) {
			scheduler.Wake(slug, "firehose:"+name)
		})
		firehose.OnCursorLoss = func() {
			for _, spec := range specs {
				scheduler.Wake(spec.Slug, "firehose-cursor-loss")
			}
		}
		firehose.Logf = logf
		logf("firehose %s: subscribing for %d repositories", name, len(dids))
		go func() { _ = firehose.Run(ctx) }()
	}

	webhooks, err := webhookPlatforms(cfg, logf)
	if err != nil {
		fmt.Fprintln(stderr, "nuncio serve:", err)
		return exitOperational
	}
	receiver := events.NewReceiver(webhooks, a.Store, scheduler)
	receiver.Logf = logf
	gate := &events.Gate{Store: a.Store, Peers: gatePeers(a), Logf: logf}

	addr := *listen
	if addr == "" {
		addr = cfg.Daemon.Listen
	}
	var server *http.Server
	var serveErr = make(chan error, 1)
	if addr != "" {
		if isPublicAddr(addr) {
			logf("warning: listening on %s; webhook signatures are verified, but you still need TLS in front of this listener", addr)
		}
		status := &statusServer{
			app: a, cfg: cfg, scheduler: scheduler, path: configPath,
			specs: specs, notices: notices, listen: addr,
		}
		mux := http.NewServeMux()
		mux.Handle("/hooks/", receiver.Handler())
		mux.Handle("/gate/", gate.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "ok")
		})
		mux.HandleFunc("/status", status.html)
		mux.HandleFunc("/status.json", status.json)
		server = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logf("listening on %s (status: http://%s/status)", addr, addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("http server: %v", err)
				serveErr <- err
				stop()
			}
		}()
	} else {
		logf("no listen address; timer%s mode only", firehoseSuffix(cfg))
	}

	fmt.Fprintf(stdout, "nuncio %s serving %d repos (dry_run=%t)\n", version, len(specs), cfg.Daemon.DryRun)
	<-ctx.Done()
	logf("shutting down")
	if server != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}
	scheduler.Stop()
	select {
	case err := <-serveErr:
		fmt.Fprintln(stderr, "nuncio serve:", err)
		return exitOperational
	default:
		return exitOK
	}
}

func firehoseSuffix(cfg *config.Config) string {
	for _, p := range cfg.Platforms {
		if p.Kind == "tangled" && p.Firehose != "" {
			return " + firehose"
		}
	}
	return ""
}

func lostSummary(run *app.Run) string {
	if !run.Lost.Complete {
		if len(run.Lost.Missing) > 0 {
			return fmt.Sprintf("unknown (%d missing)", len(run.Lost.Missing))
		}
		return "unknown"
	}
	return fmt.Sprintf("%d", run.Lost.Count)
}

func isPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return host == "" || host == "0.0.0.0" || host == "::"
}

func registerCoordinates(ctx context.Context, a *app.App, specs []app.RepoSpec) error {
	for _, spec := range specs {
		row, err := a.Store.EnsureRepo(ctx, spec.Slug, spec.Policy, time.Now().Unix())
		if err != nil {
			return err
		}
		for platform, raw := range spec.Remotes {
			parts := strings.SplitN(raw, "/", 2)
			if len(parts) != 2 {
				continue
			}
			if err := a.Store.SetRepoPlatform(ctx, state.RepoPlatform{
				RepoID: row.ID, Platform: platform, Owner: parts[0], Name: parts[1],
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func firehoseDIDs(ctx context.Context, a *app.App, platform string, specs []app.RepoSpec, logf func(string, ...any)) map[string]string {
	adapter := a.Adapters[platform]
	if adapter == nil {
		return nil
	}
	out := map[string]string{}
	for _, spec := range specs {
		if !contains(spec.Platforms, platform) {
			continue
		}
		raw := spec.Remotes[platform]
		parts := strings.SplitN(raw, "/", 2)
		if len(parts) != 2 {
			continue
		}
		repo, err := adapter.GetRepo(ctx, parts[0], parts[1])
		if err != nil {
			logf("firehose %s: cannot resolve %s: %v", platform, spec.Slug, err)
			continue
		}
		if repo.DID == "" {
			logf("firehose %s: %s has no DID in its repo record", platform, spec.Slug)
			continue
		}
		out[repo.DID] = spec.Slug
	}
	return out
}

func webhookPlatforms(cfg *config.Config, logf func(string, ...any)) (map[string]events.WebhookPlatform, error) {
	out := map[string]events.WebhookPlatform{}
	for _, name := range cfg.PlatformNames() {
		p := cfg.Platforms[name]
		if p.WebhookSecret == "" && p.WebhookPublicKey == "" {
			continue
		}
		wp := events.WebhookPlatform{Kind: p.Kind}
		if p.WebhookSecret != "" {
			value, err := config.Resolve(p.WebhookSecret)
			if err != nil {
				logf("warning: webhook secret for %s is unavailable (%v); deliveries will be refused", name, err)
			} else {
				wp.Secret = value
			}
		}
		if p.WebhookPublicKey != "" {
			pub, err := events.ParseEd25519PublicKey(p.WebhookPublicKey)
			if err != nil {
				logf("warning: webhook public key for %s is invalid: %v", name, err)
			} else {
				wp.PublicKey = ed25519.PublicKey(pub)
			}
		}
		out[name] = wp
	}
	return out, nil
}

func gatePeers(a *app.App) func(context.Context, string, string) (map[string]string, error) {
	return func(ctx context.Context, slug, ref string) (map[string]string, error) {
		row, err := a.Store.GetRepo(ctx, slug)
		if err != nil {
			return nil, err
		}
		stateByRef, err := a.Store.LoadRefState(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for platform, rs := range stateByRef[ref] {
			if rs.Present {
				out[platform] = rs.SHA
			}
		}
		return out, nil
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

type statusServer struct {
	app       *app.App
	cfg       *config.Config
	scheduler *events.Scheduler
	path      string
	specs     []app.RepoSpec
	notices   []string
	listen    string
}

type statusRepo struct {
	Slug      string             `json:"slug"`
	Policy    string             `json:"policy"`
	Platforms []string           `json:"platforms"`
	Remotes   map[string]string  `json:"remotes"`
	Generated bool               `json:"generated,omitempty"`
	Group     string             `json:"group,omitempty"`
	Scheduler *events.RepoStatus `json:"scheduler,omitempty"`
}

func (s *statusServer) snapshot() map[string]any {
	bySlug := map[string]*events.RepoStatus{}
	for _, st := range s.scheduler.Status() {
		copy := st
		bySlug[st.Slug] = &copy
	}
	repos := make([]statusRepo, 0, len(s.specs))
	for _, spec := range s.specs {
		repos = append(repos, statusRepo{
			Slug: spec.Slug, Policy: spec.Policy, Platforms: spec.Platforms,
			Remotes: spec.Remotes, Generated: spec.Generated, Group: spec.Group,
			Scheduler: bySlug[spec.Slug],
		})
	}
	payload := map[string]any{
		"version": version, "config": s.path, "dry_run": s.cfg.Daemon.DryRun,
		"listen": s.listen, "repos": repos, "now": time.Now().Unix(),
		"warnings": s.app.Warnings(),
	}
	if len(s.notices) > 0 {
		payload["notices"] = s.notices
	}
	return payload
}

func (s *statusServer) json(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.snapshot())
}

func (s *statusServer) html(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := s.snapshot()
	repos, _ := data["repos"].([]statusRepo)
	warnings, _ := data["warnings"].([]string)
	notices, _ := data["notices"].([]string)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>nuncio status</title><style>`)
	b.WriteString(`body{font:14px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:#1b1b1b;background:#f6f4ee;margin:2rem;max-width:72rem}`)
	b.WriteString(`h1{font-size:1.15rem;font-weight:600;margin:0 0 .25rem}h2{font-size:.95rem;font-weight:600;margin:2rem 0 .5rem}`)
	b.WriteString(`table{border-collapse:collapse;width:100%}th,td{text-align:left;vertical-align:top;padding:.3rem .9rem .3rem 0;border-bottom:1px solid #d9d4c8}`)
	b.WriteString(`th{font-weight:600;color:#555}code{font:inherit}.muted{color:#777}.warn{color:#8a3b12}`)
	b.WriteString(`</style></head><body>`)
	fmt.Fprintf(&b, `<h1>nuncio %s</h1>`, html.EscapeString(version))
	fmt.Fprintf(&b, `<p class="muted">config %s &middot; dry_run %t &middot; listen %s &middot; %s</p>`,
		html.EscapeString(fmt.Sprint(data["config"])), s.cfg.Daemon.DryRun,
		html.EscapeString(fmt.Sprint(data["listen"])), time.Now().Format(time.RFC3339))
	if errText, ok := data["error"].(string); ok {
		fmt.Fprintf(&b, `<p class="warn">%s</p>`, html.EscapeString(errText))
	}
	for _, warning := range warnings {
		fmt.Fprintf(&b, `<p class="warn">warning: %s</p>`, html.EscapeString(warning))
	}
	for _, notice := range notices {
		fmt.Fprintf(&b, `<p class="muted">notice: %s</p>`, html.EscapeString(notice))
	}

	b.WriteString(`<h2>repositories</h2><table><tr><th>repo</th><th>policy</th><th>platforms</th><th>last run</th><th>runs</th><th>last error</th></tr>`)
	if len(repos) == 0 {
		b.WriteString(`<tr><td colspan="6" class="muted">none managed</td></tr>`)
	}
	for _, repo := range repos {
		last, runs, lastErr := "-", "0", ""
		if repo.Scheduler != nil {
			runs = fmt.Sprint(repo.Scheduler.Runs)
			if repo.Scheduler.LastRun > 0 {
				last = time.Unix(repo.Scheduler.LastRun, 0).Format("2006-01-02 15:04:05")
			}
			lastErr = repo.Scheduler.LastError
		}
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td class="warn">%s</td></tr>`,
			html.EscapeString(repo.Slug), html.EscapeString(repo.Policy),
			html.EscapeString(strings.Join(repo.Platforms, ", ")), html.EscapeString(last),
			runs, html.EscapeString(lastErr))
	}
	b.WriteString(`</table>`)

	b.WriteString(`<h2>platforms</h2><table><tr><th>platform</th><th>kind</th><th>api / git</th><th>capabilities</th></tr>`)
	names := s.cfg.PlatformNames()
	sort.Strings(names)
	for _, name := range names {
		p := s.cfg.Platforms[name]
		var capsList []string
		if adapter := s.app.Adapters[name]; adapter != nil {
			c := adapter.Caps()
			for _, pair := range []struct {
				name string
				ok   bool
			}{
				{"push", c.GitPush}, {"force", c.GitPushForce}, {"ref-delete", c.GitRefDelete},
				{"protected", c.GitProtected}, {"lfs", c.GitLFS}, {"webhook", c.EventWebhook},
				{"firehose", c.EventFirehose}, {"gate", c.EventGate},
			} {
				if pair.ok {
					capsList = append(capsList, pair.name)
				}
			}
			if reserved := c.ReservedRefs; len(reserved) > 0 {
				capsList = append(capsList, "reserved="+strings.Join(reserved, ","))
			}
		}
		endpoint := p.API
		if endpoint == "" {
			endpoint = p.Git
		}
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			html.EscapeString(name), html.EscapeString(p.Kind), html.EscapeString(endpoint),
			html.EscapeString(strings.Join(capsList, " ")))
	}
	b.WriteString(`</table>`)
	b.WriteString(`<p class="muted">machine readable: <code>/status.json</code> &middot; health: <code>/healthz</code></p>`)
	b.WriteString(`</body></html>`)
	fmt.Fprint(w, b.String())
}
