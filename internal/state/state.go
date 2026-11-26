// Package state is nuncio's SQLite memory. It stores what was observed, what
// was attempted, and what was snapshotted. It never stores secrets.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed state store.
type Store struct {
	db   *sql.DB
	path string
}

// Repo is one managed repository.
type Repo struct {
	ID        int64
	Slug      string
	Policy    string
	CreatedAt int64
	Disabled  bool
}

// RepoPlatform binds a logical repo to its coordinates on a platform.
type RepoPlatform struct {
	RepoID          int64
	Platform        string
	Owner           string
	Name            string
	CreatedByNuncio bool
}

// RefState is the last observed state of one ref on one platform.
type RefState struct {
	Ref        string
	Platform   string
	SHA        string
	Present    bool
	ObservedAt int64
	FirstSeen  int64
	LastAction string
}

// Pair records a divergence, permanently.
type Pair struct {
	ID         int64
	RepoID     int64
	RefA       string
	SHA_A      string
	RefB       string
	SHA_B      string
	Class      string
	DetectedAt int64
	ResolvedAt int64
}

// JournalEntry is one journalled action.
type JournalEntry struct {
	ID         int64
	RepoID     int64
	GroupID    string
	Action     string
	Platform   string
	Ref        string
	FromSHA    string
	ToSHA      string
	Status     string
	StartedAt  int64
	FinishedAt int64
	Error      string
	Attempts   int
	Payload    string
}

// JournalAction is what the executor asks to be recorded before it starts.
type JournalAction struct {
	Action   string
	Platform string
	Ref      string
	FromSHA  string
	ToSHA    string
	Payload  string
}

// Snapshot is a permanent ref written to preserve a tip.
type Snapshot struct {
	ID        int64
	RepoID    int64
	Platform  string
	Ref       string
	SHA       string
	Tag       string
	CreatedAt int64
}

// ReportRecord is one stored report run.
type ReportRecord struct {
	ID         int64
	RepoID     int64
	StartedAt  int64
	FinishedAt int64
	DryRun     bool
	Lost       int64
	JSON       string
}

// Open opens (and creates) a store. WAL mode is enabled.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the file backing the store.
func (s *Store) Path() string { return s.path }

const schemaV1 = `
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS platform (
	id       INTEGER PRIMARY KEY,
	name     TEXT NOT NULL UNIQUE,
	kind     TEXT NOT NULL,
	endpoint TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS repo (
	id         INTEGER PRIMARY KEY,
	slug       TEXT NOT NULL UNIQUE,
	policy     TEXT NOT NULL DEFAULT 'default',
	created_at INTEGER NOT NULL,
	disabled   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS repo_platform (
	repo_id            INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
	platform           TEXT NOT NULL,
	remote_owner       TEXT NOT NULL,
	remote_name        TEXT NOT NULL,
	created_by_nuncio  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo_id, platform)
);
CREATE TABLE IF NOT EXISTS ref_state (
	repo_id       INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
	refname       TEXT NOT NULL,
	platform      TEXT NOT NULL,
	sha           TEXT NOT NULL DEFAULT '',
	present       INTEGER NOT NULL DEFAULT 0,
	observed_at   INTEGER NOT NULL,
	first_seen_at INTEGER NOT NULL,
	last_action   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repo_id, refname, platform)
);
CREATE TABLE IF NOT EXISTS pair (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_id     INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
	ref_a       TEXT NOT NULL,
	sha_a       TEXT NOT NULL,
	ref_b       TEXT NOT NULL,
	sha_b       TEXT NOT NULL,
	class       TEXT NOT NULL,
	detected_at INTEGER NOT NULL,
	resolved_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS journal (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_id     INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
	group_id    TEXT NOT NULL,
	action      TEXT NOT NULL,
	platform    TEXT NOT NULL DEFAULT '',
	refname     TEXT NOT NULL DEFAULT '',
	from_sha    TEXT NOT NULL DEFAULT '',
	to_sha      TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	attempts    INTEGER NOT NULL DEFAULT 0,
	payload     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS journal_group_idx ON journal(repo_id, group_id);
CREATE TABLE IF NOT EXISTS snapshot (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_id    INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
	platform   TEXT NOT NULL,
	refname    TEXT NOT NULL,
	sha        TEXT NOT NULL,
	tag_name   TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (repo_id, platform, tag_name)
);
CREATE TABLE IF NOT EXISTS report_run (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_id      INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
	started_at   INTEGER NOT NULL,
	finished_at  INTEGER NOT NULL,
	dry_run      INTEGER NOT NULL,
	lost_commits INTEGER NOT NULL DEFAULT 0,
	json         TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS webhook_seen (
	delivery_id TEXT PRIMARY KEY,
	seen_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sha_seen (
	repo_id       INTEGER NOT NULL REFERENCES repo(id) ON DELETE CASCADE,
	sha           TEXT NOT NULL,
	first_seen_at INTEGER NOT NULL,
	PRIMARY KEY (repo_id, sha)
);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaV1); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_version").Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = s.db.ExecContext(ctx, "INSERT INTO schema_version(version) VALUES (1)")
	}
	return err
}

// SchemaVersion reads the recorded version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "SELECT version FROM schema_version LIMIT 1").Scan(&v)
	return v, err
}

// EnsureRepo returns the repo row, creating it if needed.
func (s *Store) EnsureRepo(ctx context.Context, slug, policy string, now int64) (Repo, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repo(slug, policy, created_at, disabled) VALUES(?,?,?,0)
		 ON CONFLICT(slug) DO UPDATE SET policy=excluded.policy`,
		slug, policy, now)
	if err != nil {
		return Repo{}, err
	}
	return s.GetRepo(ctx, slug)
}

// GetRepo loads one repo.
func (s *Store) GetRepo(ctx context.Context, slug string) (Repo, error) {
	var r Repo
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, slug, policy, created_at, disabled FROM repo WHERE slug = ?`, slug).
		Scan(&r.ID, &r.Slug, &r.Policy, &r.CreatedAt, &disabled)
	r.Disabled = disabled != 0
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, fmt.Errorf("repo %q is not in state", slug)
	}
	return r, err
}

// ListRepos lists managed repos.
func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, slug, policy, created_at, disabled FROM repo ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		var disabled int
		if err := rows.Scan(&r.ID, &r.Slug, &r.Policy, &r.CreatedAt, &disabled); err != nil {
			return nil, err
		}
		r.Disabled = disabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRepoPlatform records the remote coordinates for a platform.
func (s *Store) SetRepoPlatform(ctx context.Context, rp RepoPlatform) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repo_platform(repo_id, platform, remote_owner, remote_name, created_by_nuncio)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(repo_id, platform) DO UPDATE SET
		   remote_owner=excluded.remote_owner,
		   remote_name=excluded.remote_name,
		   created_by_nuncio=excluded.created_by_nuncio`,
		rp.RepoID, rp.Platform, rp.Owner, rp.Name, boolInt(rp.CreatedByNuncio))
	return err
}

// RepoPlatforms lists coordinates for a repo.
func (s *Store) RepoPlatforms(ctx context.Context, repoID int64) ([]RepoPlatform, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo_id, platform, remote_owner, remote_name, created_by_nuncio
		 FROM repo_platform WHERE repo_id = ? ORDER BY platform`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoPlatform
	for rows.Next() {
		var rp RepoPlatform
		var byNuncio int
		if err := rows.Scan(&rp.RepoID, &rp.Platform, &rp.Owner, &rp.Name, &byNuncio); err != nil {
			return nil, err
		}
		rp.CreatedByNuncio = byNuncio != 0
		out = append(out, rp)
	}
	return out, rows.Err()
}

// FindRepoByRemote resolves a webhook's repository coordinates back to a slug.
func (s *Store) FindRepoByRemote(ctx context.Context, platform, owner, name string) (Repo, error) {
	var r Repo
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT r.id, r.slug, r.policy, r.created_at, r.disabled
		 FROM repo r JOIN repo_platform rp ON rp.repo_id = r.id
		 WHERE rp.platform = ? AND lower(rp.remote_owner) = lower(?) AND lower(rp.remote_name) = lower(?)`,
		platform, owner, name).Scan(&r.ID, &r.Slug, &r.Policy, &r.CreatedAt, &disabled)
	r.Disabled = disabled != 0
	return r, err
}

// RecordObservation upserts one platform's view of one ref and remembers the
// SHA forever.
func (s *Store) RecordObservation(ctx context.Context, repoID int64, refname, platform, sha string, present bool, now int64, lastAction string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ref_state(repo_id, refname, platform, sha, present, observed_at, first_seen_at, last_action)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(repo_id, refname, platform) DO UPDATE SET
		   sha = excluded.sha,
		   present = excluded.present,
		   observed_at = excluded.observed_at,
		   last_action = CASE WHEN excluded.last_action = '' THEN ref_state.last_action ELSE excluded.last_action END`,
		repoID, refname, platform, sha, boolInt(present), now, now, lastAction)
	if err != nil {
		return err
	}
	if present && sha != "" {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO sha_seen(repo_id, sha, first_seen_at) VALUES(?,?,?)
			 ON CONFLICT(repo_id, sha) DO NOTHING`, repoID, sha, now)
	}
	return err
}

// MarkAbsent records that a previously known ref was not observed.
func (s *Store) MarkAbsent(ctx context.Context, repoID int64, refname, platform string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ref_state SET present = 0, observed_at = ?
		 WHERE repo_id = ? AND refname = ? AND platform = ?`, now, repoID, refname, platform)
	return err
}

// LoadRefState returns prior state grouped by ref then platform.
func (s *Store) LoadRefState(ctx context.Context, repoID int64) (map[string]map[string]RefState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT refname, platform, sha, present, observed_at, first_seen_at, last_action
		 FROM ref_state WHERE repo_id = ?`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]RefState{}
	for rows.Next() {
		var rs RefState
		var present int
		if err := rows.Scan(&rs.Ref, &rs.Platform, &rs.SHA, &present, &rs.ObservedAt, &rs.FirstSeen, &rs.LastAction); err != nil {
			return nil, err
		}
		rs.Present = present != 0
		if out[rs.Ref] == nil {
			out[rs.Ref] = map[string]RefState{}
		}
		out[rs.Ref][rs.Platform] = rs
	}
	return out, rows.Err()
}

// UniverseSHAs returns every SHA ever observed in this repo.
func (s *Store) UniverseSHAs(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha FROM sha_seen WHERE repo_id = ? ORDER BY sha`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

// BeginGroup writes a group of planned actions and returns their ids.
func (s *Store) BeginGroup(ctx context.Context, repoID int64, groupID string, actions []JournalAction, now int64) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := make([]int64, 0, len(actions))
	for _, a := range actions {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO journal(repo_id, group_id, action, platform, refname, from_sha, to_sha, status, started_at, payload)
			 VALUES(?,?,?,?,?,?,?,'planned',?,?)`,
			repoID, groupID, a.Action, a.Platform, a.Ref, a.FromSHA, a.ToSHA, now, a.Payload)
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	return ids, tx.Commit()
}

// MarkAction records an outcome.
func (s *Store) MarkAction(ctx context.Context, id int64, status, errText string, attempts int, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE journal SET status = ?, error = ?, attempts = ?, finished_at = ?,
		 started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END
		 WHERE id = ?`, status, errText, attempts, now, now, id)
	return err
}

// MarkActionRunning records that execution started.
func (s *Store) MarkActionRunning(ctx context.Context, id int64, attempts int, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE journal SET status = 'running', attempts = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END
		 WHERE id = ?`, attempts, now, id)
	return err
}

// PendingActions returns unfinished entries for a repo, oldest first.
func (s *Store) PendingActions(ctx context.Context, repoID int64) ([]JournalEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, group_id, action, platform, refname, from_sha, to_sha, status,
		        started_at, finished_at, error, attempts, payload
		 FROM journal WHERE repo_id = ? AND status IN ('planned','running') ORDER BY id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.ID, &e.RepoID, &e.GroupID, &e.Action, &e.Platform, &e.Ref,
			&e.FromSHA, &e.ToSHA, &e.Status, &e.StartedAt, &e.FinishedAt, &e.Error, &e.Attempts, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GroupOf returns the group id for one journal entry.
func (s *Store) GroupOf(ctx context.Context, id int64) (string, error) {
	var group string
	err := s.db.QueryRowContext(ctx, `SELECT group_id FROM journal WHERE id = ?`, id).Scan(&group)
	return group, err
}

// GroupActions lists one group.
func (s *Store) GroupActions(ctx context.Context, groupID string) ([]JournalEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, group_id, action, platform, refname, from_sha, to_sha, status,
		        started_at, finished_at, error, attempts, payload
		 FROM journal WHERE group_id = ? ORDER BY id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		if err := rows.Scan(&e.ID, &e.RepoID, &e.GroupID, &e.Action, &e.Platform, &e.Ref,
			&e.FromSHA, &e.ToSHA, &e.Status, &e.StartedAt, &e.FinishedAt, &e.Error, &e.Attempts, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordPair stores one divergence fact.
func (s *Store) RecordPair(ctx context.Context, p Pair) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pair(repo_id, ref_a, sha_a, ref_b, sha_b, class, detected_at, resolved_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		p.RepoID, p.RefA, p.SHA_A, p.RefB, p.SHA_B, p.Class, p.DetectedAt, p.ResolvedAt)
	return err
}

// ListPairs lists divergences for a repo.
func (s *Store) ListPairs(ctx context.Context, repoID int64) ([]Pair, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, ref_a, sha_a, ref_b, sha_b, class, detected_at, resolved_at
		 FROM pair WHERE repo_id = ? ORDER BY detected_at DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pair
	for rows.Next() {
		var p Pair
		if err := rows.Scan(&p.ID, &p.RepoID, &p.RefA, &p.SHA_A, &p.RefB, &p.SHA_B, &p.Class, &p.DetectedAt, &p.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordSnapshot remembers a snapshot ref that was written.
func (s *Store) RecordSnapshot(ctx context.Context, repoID int64, platform, refname, sha, tag string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO snapshot(repo_id, platform, refname, sha, tag_name, created_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(repo_id, platform, tag_name) DO UPDATE SET sha = excluded.sha`,
		repoID, platform, refname, sha, tag, now)
	return err
}

// ListSnapshots lists snapshots for a repo.
func (s *Store) ListSnapshots(ctx context.Context, repoID int64) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, platform, refname, sha, tag_name, created_at
		 FROM snapshot WHERE repo_id = ? ORDER BY created_at DESC, tag_name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		if err := rows.Scan(&sn.ID, &sn.RepoID, &sn.Platform, &sn.Ref, &sn.SHA, &sn.Tag, &sn.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// FindSnapshot finds one snapshot by its ref name.
func (s *Store) FindSnapshot(ctx context.Context, repoID int64, tag string) (Snapshot, error) {
	var sn Snapshot
	err := s.db.QueryRowContext(ctx,
		`SELECT id, repo_id, platform, refname, sha, tag_name, created_at
		 FROM snapshot WHERE repo_id = ? AND tag_name = ?`, repoID, tag).Scan(
		&sn.ID, &sn.RepoID, &sn.Platform, &sn.Ref, &sn.SHA, &sn.Tag, &sn.CreatedAt)
	return sn, err
}

// SnapshotSHAs returns every snapshot SHA for a repo.
func (s *Store) SnapshotSHAs(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT sha FROM snapshot WHERE repo_id = ?`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

// RecordReport stores one report run.
func (s *Store) RecordReport(ctx context.Context, r ReportRecord) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO report_run(repo_id, started_at, finished_at, dry_run, lost_commits, json)
		 VALUES(?,?,?,?,?,?)`,
		r.RepoID, r.StartedAt, r.FinishedAt, boolInt(r.DryRun), r.Lost, r.JSON)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LatestReport loads the newest report for a repo.
func (s *Store) LatestReport(ctx context.Context, repoID int64) (ReportRecord, error) {
	var r ReportRecord
	var dry int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, repo_id, started_at, finished_at, dry_run, lost_commits, json
		 FROM report_run WHERE repo_id = ? ORDER BY id DESC LIMIT 1`, repoID).Scan(
		&r.ID, &r.RepoID, &r.StartedAt, &r.FinishedAt, &dry, &r.Lost, &r.JSON)
	r.DryRun = dry != 0
	return r, err
}

// ReportsSince lists stored reports that started at or after since.
func (s *Store) ReportsSince(ctx context.Context, repoID int64, since int64) ([]ReportRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, started_at, finished_at, dry_run, lost_commits, json
		 FROM report_run WHERE repo_id = ? AND started_at >= ? ORDER BY id DESC`, repoID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReportRecord
	for rows.Next() {
		var r ReportRecord
		var dry int
		if err := rows.Scan(&r.ID, &r.RepoID, &r.StartedAt, &r.FinishedAt, &dry, &r.Lost, &r.JSON); err != nil {
			return nil, err
		}
		r.DryRun = dry != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkWebhookSeen records a delivery id and reports whether it is new.
func (s *Store) MarkWebhookSeen(ctx context.Context, deliveryID string, now int64) (bool, error) {
	if deliveryID == "" {
		return true, nil
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_seen(delivery_id, seen_at) VALUES(?,?) ON CONFLICT(delivery_id) DO NOTHING`,
		deliveryID, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpsertPlatform records a platform endpoint for introspection.
func (s *Store) UpsertPlatform(ctx context.Context, name, kind, endpoint string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO platform(name, kind, endpoint) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET kind=excluded.kind, endpoint=excluded.endpoint`,
		name, kind, endpoint)
	return err
}

// PruneJournal removes finished entries older than the given time. It exists so
// a long-running daemon does not grow forever; unfinished entries are never
// removed.
func (s *Store) PruneJournal(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM journal WHERE status IN ('done','skipped','failed') AND finished_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NewGroupID makes a deterministic-ish group identifier from time and sequence.
func NewGroupID(now time.Time, seq int) string {
	return fmt.Sprintf("%d-%d", now.UnixNano(), seq)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NormalizeSlug validates and normalises owner/name.
func NormalizeSlug(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.Count(s, "/") != 1 {
		return "", fmt.Errorf("repo must be owner/name, got %q", s)
	}
	return s, nil
}
