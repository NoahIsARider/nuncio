package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/NoahIsARider/nuncio/internal/caps"
	"github.com/NoahIsARider/nuncio/internal/forge"
)

// DoctorRow is one platform's capability matrix as this operator's credentials
// actually behave.
type DoctorRow struct {
	Platform  string
	Kind      string
	API       string
	Push      string
	Force     string
	Delete    string
	Webhook   string
	Gate      string
	Firehose  string
	Protected string
	Reserved  string
	Err       string
}

// Doctor verifies every configured platform and reports its capabilities. It
// never prints a secret value.
func (a *App) Doctor(ctx context.Context) []DoctorRow {
	var rows []DoctorRow
	for _, name := range a.Config.PlatformNames() {
		p := a.Config.Platforms[name]
		adapter := a.Adapters[name]
		row := DoctorRow{Platform: name, Kind: p.Kind}
		row.API = "n/a"
		if adapter != nil && adapter.Kind() != string(caps.KindGit) {
			if err := adapter.Verify(ctx); err != nil {
				row.API = "FAIL"
				row.Err = err.Error()
			} else {
				row.API = "ok"
			}
		}
		var c caps.Capabilities
		if adapter != nil {
			c = adapter.Caps()
		}
		row.Push = yesNo(c.GitPush)
		row.Force = yesNo(c.GitPushForce)
		row.Delete = yesNo(c.GitRefDelete)
		row.Protected = yesNo(c.GitProtected)
		row.Reserved = reservedFor(c)
		if c.EventWebhook {
			if p.WebhookSecret == "" && p.WebhookPublicKey == "" {
				row.Webhook = "no secret"
			} else {
				row.Webhook = "ok"
			}
		} else {
			row.Webhook = "-"
		}
		if c.EventFirehose {
			row.Firehose = "ok"
		} else {
			row.Firehose = "-"
		}
		if c.EventGate {
			if p.PreReceiveGate {
				// S1 is not verified. Saying "ok" here would be a lie in the one
				// place operators look first.
				row.Gate = "UNVERIFIED"
			} else {
				row.Gate = "available"
			}
		} else {
			row.Gate = "-"
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Platform < rows[j].Platform })
	return rows
}

// Discover lists repositories from one platform, or from every platform that
// has a repository API.
func (a *App) Discover(ctx context.Context, platform string) (map[string][]forge.Repo, error) {
	out := map[string][]forge.Repo{}
	names := a.Config.PlatformNames()
	if platform != "" {
		if _, ok := a.Adapters[platform]; !ok {
			return nil, fmt.Errorf("platform %q is not configured", platform)
		}
		names = []string{platform}
	}
	for _, name := range names {
		adapter := a.Adapters[name]
		if adapter == nil || adapter.Kind() == string(caps.KindGit) {
			continue
		}
		repos, err := adapter.ListRepos(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", name, err)
		}
		sort.Slice(repos, func(i, j int) bool {
			return repos[i].Owner+"/"+repos[i].Name < repos[j].Owner+"/"+repos[j].Name
		})
		out[name] = repos
	}
	return out, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// FormatDoctor renders the doctor matrix as aligned text.
func FormatDoctor(rows []DoctorRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-10s %-8s api %-4s push %-3s force %-3s ref-delete %-3s",
			r.Platform, r.Kind, r.API, r.Push, r.Force, r.Delete)
		if r.Webhook != "-" {
			fmt.Fprintf(&b, " webhook %s", r.Webhook)
		}
		if r.Firehose != "-" {
			fmt.Fprintf(&b, " firehose %s", r.Firehose)
		}
		if r.Gate != "-" {
			fmt.Fprintf(&b, " gate %s", r.Gate)
		}
		if r.Protected != "no" {
			fmt.Fprintf(&b, " protected %s", r.Protected)
		}
		if r.Reserved != "-" {
			fmt.Fprintf(&b, " reserved %s", r.Reserved)
		}
		b.WriteString("\n")
		if r.Err != "" {
			fmt.Fprintf(&b, "           error: %s\n", r.Err)
		}
	}
	return b.String()
}
