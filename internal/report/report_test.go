package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NoahIsARider/nuncio/internal/reconcile"
	"github.com/NoahIsARider/nuncio/internal/state"
)

func sample() Data {
	ref := "refs/heads/feat/http2"
	snap := reconcile.SnapshotRef(ref, 1759000000)
	return Data{
		Repo: "alice/example",
		Platforms: []PlatformInfo{
			{Name: "github", Coordinates: "alice/example"},
			{Name: "codeberg", Coordinates: "alice/example"},
			{Name: "tangled", Coordinates: "alice.tngl.sh/example"},
		},
		GeneratedAt: time.Unix(1759000000, 0),
		DryRun:      false,
		Results: []reconcile.RefResult{
			{
				Ref: "refs/heads/main", Class: reconcile.ClassEqual,
				Peers: []reconcile.Peer{
					{Platform: "github", Present: true, SHA: "aaaaaaa1"},
					{Platform: "codeberg", Present: true, SHA: "aaaaaaa1"},
					{Platform: "tangled", Present: true, SHA: "aaaaaaa1"},
				},
			},
			{
				Ref: ref, Class: reconcile.ClassDivergence, Base: "bbbbbbb0",
				Peers: []reconcile.Peer{
					{Platform: "github", Present: true, SHA: "9d3e0b1"},
					{Platform: "codeberg", Present: true, SHA: "4f1a2c0"},
					{Platform: "tangled", Present: true, SHA: "4f1a2c0"},
				},
				Actions: []reconcile.Action{
					{Kind: reconcile.ActionSnapshot, Platform: "github", Ref: ref, FromSHA: "9d3e0b1", SnapshotRef: snap},
					{Kind: reconcile.ActionSnapshot, Platform: "codeberg", Ref: ref, FromSHA: "4f1a2c0", SnapshotRef: snap},
				},
				Notices: []reconcile.Notice{{Kind: "park", Text: "authority is intent, not a licence"}},
			},
			{
				Ref: "refs/pull/12/head", Class: reconcile.ClassNamespaceRejected,
				Peers:   []reconcile.Peer{{Platform: "github", Present: true, SHA: "ccccccc1"}},
				Blocked: "namespace:refs/pull/*",
				Notices: []reconcile.Notice{{Platform: "codeberg", Kind: "namespace", Text: "reserved namespace refs/pull/*"}},
			},
		},
		Snapshots: []state.Snapshot{
			{Platform: "github", Ref: ref, SHA: "9d3e0b1", Tag: snap, CreatedAt: 1759000000},
			{Platform: "codeberg", Ref: ref, SHA: "4f1a2c0", Tag: snap, CreatedAt: 1759000000},
		},
		Lost: Lost{Count: 0, Complete: true},
	}
}

func TestHumanReport(t *testing.T) {
	var buf bytes.Buffer
	d := sample()
	if err := d.RenderHuman(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"alice/example", "github:alice/example", "tangled:alice.tngl.sh/example",
		"refs/heads/main", "equal", "refs/heads/feat/http2", "divergence", "parked",
		"namespace", "reserved namespace refs/pull/*",
		"lost commits        0",
		"refs/nuncio/snapshot/feat/http2/1759000000 (codeberg, github)",
		"to resolve",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q\n%s", want, out)
		}
	}
	// The namespace ref is blocked, so it must be listed under to resolve.
	if !strings.Contains(out, "refs/pull/12/head") {
		t.Errorf("namespace ref not in to-resolve line:\n%s", out)
	}
}

func TestIncompleteLostIsNeverZero(t *testing.T) {
	var buf bytes.Buffer
	d := sample()
	d.Lost = Lost{Complete: false, Missing: []string{"deadbeef", "cafebabe"}}
	if err := d.RenderHuman(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "lost commits        0") || !strings.Contains(out, "unknown") {
		t.Fatalf("incomplete lost reported badly:\n%s", out)
	}
	var js bytes.Buffer
	if err := d.RenderJSON(&js); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["lost_commits"] != nil {
		t.Errorf("lost_commits = %v, want null when incomplete", decoded["lost_commits"])
	}
	if decoded["lost_complete"] != false {
		t.Errorf("lost_complete = %v", decoded["lost_complete"])
	}
}

func TestJSONReport(t *testing.T) {
	var buf bytes.Buffer
	d := sample()
	if err := d.RenderJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Version      int                   `json:"version"`
		Repo         string                `json:"repo"`
		Refs         []reconcile.RefResult `json:"refs"`
		LostCommits  *int                  `json:"lost_commits"`
		LostComplete bool                  `json:"lost_complete"`
		Snapshots    []map[string]any      `json:"snapshots"`
		Findings     int                   `json:"findings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Version != 1 || decoded.Repo != "alice/example" {
		t.Errorf("header = %+v", decoded)
	}
	if decoded.LostCommits == nil || *decoded.LostCommits != 0 {
		t.Errorf("lost_commits = %v", decoded.LostCommits)
	}
	if len(decoded.Refs) != 3 || len(decoded.Snapshots) != 2 {
		t.Errorf("refs=%d snapshots=%d", len(decoded.Refs), len(decoded.Snapshots))
	}
	if decoded.Findings == 0 {
		t.Error("findings should be non-zero for divergence and namespace")
	}
}

func TestDryRunBanner(t *testing.T) {
	var buf bytes.Buffer
	d := sample()
	d.DryRun = true
	d.RenderHuman(&buf)
	if !strings.HasPrefix(buf.String(), "alice/example (dry run)") {
		t.Errorf("dry-run banner missing:\n%s", buf.String())
	}
}
