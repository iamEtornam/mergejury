// Package store persists runs, findings, clusters, verdicts and outcomes to
// SQLite, and answers the queries the CLI and web console need.
package store

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"mergejury/internal/finding"
)

//go:embed all:migrations
var embeddedMigrations embed.FS

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// modernc/sqlite serializes at the driver level; one writer avoids
	// SQLITE_BUSY churn from concurrent adapter persistence.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(embeddedMigrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := fs.ReadFile(embeddedMigrations, "migrations/"+name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ---- runs ----

type Run struct {
	ID                int64      `json:"id"`
	Repo              string     `json:"repo"`
	PRNumber          int        `json:"pr_number"`
	HeadSHA           string     `json:"head_sha"`
	BaseSHA           string     `json:"base_sha"`
	Trigger           string     `json:"trigger"`
	Status            string     `json:"status"`
	ReviewEvent       string     `json:"review_event"`
	ReviewEventReason string     `json:"review_event_reason"`
	StartedAt         string     `json:"started_at"`
	FinishedAt        *string    `json:"finished_at"`
	TotalCostUSD      float64    `json:"total_cost_usd"`
	ConfigSnapshot    string     `json:"config_snapshot"`
	FindingsProduced  int        `json:"findings_produced"`
	CommentsPosted    int        `json:"comments_posted"`
}

func (s *Store) CreateRun(repo string, pr int, headSHA, baseSHA, trigger, configSnapshot string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO runs (repo, pr_number, head_sha, base_sha, trigger, started_at, config_snapshot)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, repo, pr, headSHA, baseSHA, trigger, now(), configSnapshot)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishRun(id int64, status, event, reason string, totalCost float64) error {
	_, err := s.db.Exec(`UPDATE runs SET status=?, review_event=?, review_event_reason=?, finished_at=?, total_cost_usd=? WHERE id=?`,
		status, event, reason, now(), totalCost, id)
	return err
}

func (s *Store) ListRuns(repo string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT r.id, r.repo, r.pr_number, r.head_sha, r.base_sha, r.trigger, r.status, r.review_event,
	             r.review_event_reason, r.started_at, r.finished_at, r.total_cost_usd, r.config_snapshot,
	             (SELECT COUNT(*) FROM findings f JOIN adapter_runs ar ON f.adapter_run_id = ar.id WHERE ar.run_id = r.id),
	             (SELECT COUNT(*) FROM verdicts v JOIN clusters c ON v.cluster_id = c.id WHERE c.run_id = r.id AND v.posted_comment_id IS NOT NULL)
	      FROM runs r`
	args := []any{}
	if repo != "" {
		q += ` WHERE r.repo = ?`
		args = append(args, repo)
	}
	q += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.Repo, &r.PRNumber, &r.HeadSHA, &r.BaseSHA, &r.Trigger, &r.Status, &r.ReviewEvent,
			&r.ReviewEventReason, &r.StartedAt, &r.FinishedAt, &r.TotalCostUSD, &r.ConfigSnapshot,
			&r.FindingsProduced, &r.CommentsPosted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRun(id int64) (*Run, error) {
	rows, err := s.db.Query(`SELECT r.id, r.repo, r.pr_number, r.head_sha, r.base_sha, r.trigger, r.status, r.review_event,
	        r.review_event_reason, r.started_at, r.finished_at, r.total_cost_usd, r.config_snapshot,
	        (SELECT COUNT(*) FROM findings f JOIN adapter_runs ar ON f.adapter_run_id = ar.id WHERE ar.run_id = r.id),
	        (SELECT COUNT(*) FROM verdicts v JOIN clusters c ON v.cluster_id = c.id WHERE c.run_id = r.id AND v.posted_comment_id IS NOT NULL)
	   FROM runs r WHERE r.id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	var r Run
	if err := rows.Scan(&r.ID, &r.Repo, &r.PRNumber, &r.HeadSHA, &r.BaseSHA, &r.Trigger, &r.Status, &r.ReviewEvent,
		&r.ReviewEventReason, &r.StartedAt, &r.FinishedAt, &r.TotalCostUSD, &r.ConfigSnapshot,
		&r.FindingsProduced, &r.CommentsPosted); err != nil {
		return nil, err
	}
	return &r, nil
}

// ---- adapter runs ----

type AdapterRun struct {
	ID           int64   `json:"id"`
	RunID        int64   `json:"run_id"`
	AdapterID    string  `json:"adapter_id"`
	Lens         string  `json:"lens"`
	Model        string  `json:"model"`
	Status       string  `json:"status"`
	DurationMS   int64   `json:"duration_ms"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	RawOutput    string  `json:"raw_output"`
	Error        string  `json:"error"`
}

func (s *Store) InsertAdapterRun(a AdapterRun) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO adapter_runs (run_id, adapter_id, lens, model, status, duration_ms, cost_usd, input_tokens, output_tokens, raw_output, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, a.AdapterID, a.Lens, a.Model, a.Status, a.DurationMS, a.CostUSD, a.InputTokens, a.OutputTokens, a.RawOutput, a.Error)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---- findings ----

type StoredFinding struct {
	ID           int64           `json:"id"`
	AdapterRunID int64           `json:"adapter_run_id"`
	AdapterID    string          `json:"adapter_id"`
	Lens         string          `json:"lens"`
	Finding      finding.Finding `json:"finding"`
	Kept         bool            `json:"kept"`
	DropReason   string          `json:"drop_reason"`
}

func (s *Store) InsertFinding(adapterRunID int64, f finding.Finding, kept bool, dropReason string) (int64, error) {
	ev, _ := json.Marshal(f.Evidence)
	res, err := s.db.Exec(`INSERT INTO findings (adapter_run_id, path, line, start_line, category, severity, title, body, suggested_patch, evidence_json, confidence, kept, drop_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		adapterRunID, f.Path, f.Line, f.StartLine, string(f.Category), string(f.Severity), f.Title, f.Body, f.SuggestedPatch, string(ev), string(f.Confidence), boolInt(kept), dropReason)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FindingsForRun(runID int64, keptOnly bool) ([]StoredFinding, error) {
	q := `SELECT f.id, f.adapter_run_id, ar.adapter_id, ar.lens, f.path, f.line, f.start_line, f.category, f.severity,
	             f.title, f.body, f.suggested_patch, f.evidence_json, f.confidence, f.kept, f.drop_reason
	      FROM findings f JOIN adapter_runs ar ON f.adapter_run_id = ar.id WHERE ar.run_id = ?`
	if keptOnly {
		q += ` AND f.kept = 1`
	}
	q += ` ORDER BY f.id`
	rows, err := s.db.Query(q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredFinding
	for rows.Next() {
		var sf StoredFinding
		var evJSON string
		var kept int
		var cat, sev, conf string
		if err := rows.Scan(&sf.ID, &sf.AdapterRunID, &sf.AdapterID, &sf.Lens, &sf.Finding.Path, &sf.Finding.Line,
			&sf.Finding.StartLine, &cat, &sev, &sf.Finding.Title, &sf.Finding.Body, &sf.Finding.SuggestedPatch,
			&evJSON, &conf, &kept, &sf.DropReason); err != nil {
			return nil, err
		}
		sf.Finding.Category = finding.Category(cat)
		sf.Finding.Severity = finding.Severity(sev)
		sf.Finding.Confidence = finding.Confidence(conf)
		sf.Finding.ReviewerID = sf.AdapterID
		sf.Finding.Lens = sf.Lens
		_ = json.Unmarshal([]byte(evJSON), &sf.Finding.Evidence)
		sf.Kept = kept == 1
		out = append(out, sf)
	}
	return out, rows.Err()
}

// ---- clusters, verifications, challenges, verdicts ----

type StoredCluster struct {
	ID             int64   `json:"id"`
	RunID          int64   `json:"run_id"`
	Path           string  `json:"path"`
	Line           int     `json:"line"`
	Category       string  `json:"category"`
	SupporterCount int     `json:"supporter_count"`
	FindingIDs     []int64 `json:"finding_ids"`
}

func (s *Store) InsertCluster(runID int64, path string, line int, category string, supporterCount int, findingIDs []int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO clusters (run_id, path, line, category, supporter_count) VALUES (?, ?, ?, ?, ?)`,
		runID, path, line, category, supporterCount)
	if err != nil {
		return 0, err
	}
	cid, _ := res.LastInsertId()
	for _, fid := range findingIDs {
		if _, err := s.db.Exec(`INSERT INTO cluster_findings (cluster_id, finding_id) VALUES (?, ?)`, cid, fid); err != nil {
			return 0, err
		}
	}
	return cid, nil
}

func (s *Store) ClustersForRun(runID int64) ([]StoredCluster, error) {
	rows, err := s.db.Query(`SELECT id, run_id, path, line, category, supporter_count FROM clusters WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredCluster
	for rows.Next() {
		var c StoredCluster
		if err := rows.Scan(&c.ID, &c.RunID, &c.Path, &c.Line, &c.Category, &c.SupporterCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		frows, err := s.db.Query(`SELECT finding_id FROM cluster_findings WHERE cluster_id = ?`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for frows.Next() {
			var fid int64
			if err := frows.Scan(&fid); err != nil {
				frows.Close()
				return nil, err
			}
			out[i].FindingIDs = append(out[i].FindingIDs, fid)
		}
		frows.Close()
	}
	return out, nil
}

// DeleteAdjudication clears clusters/challenges/verifications/verdicts for a
// run so replay can rebuild them from stored findings.
func (s *Store) DeleteAdjudication(runID int64) error {
	stmts := []string{
		`DELETE FROM outcomes WHERE verdict_id IN (SELECT v.id FROM verdicts v JOIN clusters c ON v.cluster_id=c.id WHERE c.run_id=?)`,
		`DELETE FROM verdicts WHERE cluster_id IN (SELECT id FROM clusters WHERE run_id=?)`,
		`DELETE FROM challenges WHERE cluster_id IN (SELECT id FROM clusters WHERE run_id=?)`,
		`DELETE FROM verifications WHERE cluster_id IN (SELECT id FROM clusters WHERE run_id=?)`,
		`DELETE FROM cluster_findings WHERE cluster_id IN (SELECT id FROM clusters WHERE run_id=?)`,
		`DELETE FROM clusters WHERE run_id=?`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q, runID); err != nil {
			return err
		}
	}
	return nil
}

type StoredVerification struct {
	ID         int64  `json:"id"`
	ClusterID  int64  `json:"cluster_id"`
	Kind       string `json:"kind"`
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"`
	Conclusion string `json:"conclusion"`
}

func (s *Store) InsertVerification(v StoredVerification) error {
	_, err := s.db.Exec(`INSERT INTO verifications (cluster_id, kind, command, exit_code, output, conclusion) VALUES (?, ?, ?, ?, ?, ?)`,
		v.ClusterID, v.Kind, v.Command, v.ExitCode, v.Output, v.Conclusion)
	return err
}

func (s *Store) VerificationsForRun(runID int64) ([]StoredVerification, error) {
	rows, err := s.db.Query(`SELECT v.id, v.cluster_id, v.kind, v.command, v.exit_code, v.output, v.conclusion
		FROM verifications v JOIN clusters c ON v.cluster_id = c.id WHERE c.run_id = ? ORDER BY v.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredVerification
	for rows.Next() {
		var v StoredVerification
		if err := rows.Scan(&v.ID, &v.ClusterID, &v.Kind, &v.Command, &v.ExitCode, &v.Output, &v.Conclusion); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type StoredChallenge struct {
	ID         int64  `json:"id"`
	ClusterID  int64  `json:"cluster_id"`
	Model      string `json:"model"`
	Argument   string `json:"argument"`
	CouldArgue bool   `json:"could_argue"`
}

func (s *Store) InsertChallenge(c StoredChallenge) error {
	_, err := s.db.Exec(`INSERT INTO challenges (cluster_id, model, argument, could_argue) VALUES (?, ?, ?, ?)`,
		c.ClusterID, c.Model, c.Argument, boolInt(c.CouldArgue))
	return err
}

func (s *Store) ChallengesForRun(runID int64) ([]StoredChallenge, error) {
	rows, err := s.db.Query(`SELECT ch.id, ch.cluster_id, ch.model, ch.argument, ch.could_argue
		FROM challenges ch JOIN clusters c ON ch.cluster_id = c.id WHERE c.run_id = ? ORDER BY ch.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredChallenge
	for rows.Next() {
		var c StoredChallenge
		var could int
		if err := rows.Scan(&c.ID, &c.ClusterID, &c.Model, &c.Argument, &could); err != nil {
			return nil, err
		}
		c.CouldArgue = could == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

type StoredVerdict struct {
	ID              int64   `json:"id"`
	ClusterID       int64   `json:"cluster_id"`
	Verdict         string  `json:"verdict"`
	Reason          string  `json:"reason"`
	FinalSeverity   string  `json:"final_severity"`
	FinalBody       string  `json:"final_body"`
	FinalPatch      *string `json:"final_patch"`
	PostedCommentID *int64  `json:"posted_comment_id"`
	PostedAt        *string `json:"posted_at"`
}

func (s *Store) InsertVerdict(v StoredVerdict) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO verdicts (cluster_id, verdict, reason, final_severity, final_body, final_patch) VALUES (?, ?, ?, ?, ?, ?)`,
		v.ClusterID, v.Verdict, v.Reason, v.FinalSeverity, v.FinalBody, v.FinalPatch)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) MarkVerdictPosted(verdictID, commentID int64) error {
	_, err := s.db.Exec(`UPDATE verdicts SET posted_comment_id=?, posted_at=? WHERE id=?`, commentID, now(), verdictID)
	return err
}

func (s *Store) VerdictsForRun(runID int64) ([]StoredVerdict, error) {
	rows, err := s.db.Query(`SELECT v.id, v.cluster_id, v.verdict, v.reason, v.final_severity, v.final_body, v.final_patch, v.posted_comment_id, v.posted_at
		FROM verdicts v JOIN clusters c ON v.cluster_id = c.id WHERE c.run_id = ? ORDER BY v.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredVerdict
	for rows.Next() {
		var v StoredVerdict
		if err := rows.Scan(&v.ID, &v.ClusterID, &v.Verdict, &v.Reason, &v.FinalSeverity, &v.FinalBody, &v.FinalPatch, &v.PostedCommentID, &v.PostedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- adapter runs for a run ----

func (s *Store) AdapterRunsForRun(runID int64) ([]AdapterRun, error) {
	rows, err := s.db.Query(`SELECT id, run_id, adapter_id, lens, model, status, duration_ms, cost_usd, input_tokens, output_tokens, raw_output, error
		FROM adapter_runs WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdapterRun
	for rows.Next() {
		var a AdapterRun
		if err := rows.Scan(&a.ID, &a.RunID, &a.AdapterID, &a.Lens, &a.Model, &a.Status, &a.DurationMS, &a.CostUSD, &a.InputTokens, &a.OutputTokens, &a.RawOutput, &a.Error); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- posted reviews (idempotency) ----

func (s *Store) LastPostedReview(repo string, pr int) (headSHA, event string, reviewID int64, err error) {
	err = s.db.QueryRow(`SELECT head_sha, event, review_id FROM posted_reviews WHERE repo=? AND pr_number=? ORDER BY id DESC LIMIT 1`,
		repo, pr).Scan(&headSHA, &event, &reviewID)
	if err == sql.ErrNoRows {
		return "", "", 0, nil
	}
	return
}

func (s *Store) RecordPostedReview(runID int64, repo string, pr int, headSHA, event string, reviewID int64) error {
	_, err := s.db.Exec(`INSERT INTO posted_reviews (run_id, repo, pr_number, head_sha, event, review_id, posted_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, repo, pr, headSHA, event, reviewID, now())
	return err
}

// ---- outcomes ----

func (s *Store) InsertOutcome(verdictID int64, commentState string, resolved, dismissed bool, replyCount int) error {
	_, err := s.db.Exec(`INSERT INTO outcomes (verdict_id, comment_state, resolved, dismissed, reply_count, observed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		verdictID, commentState, boolInt(resolved), boolInt(dismissed), replyCount, now())
	return err
}

// ---- stats ----

type AdapterStats struct {
	AdapterID        string  `json:"adapter_id"`
	Lens             string  `json:"lens"`
	Runs             int     `json:"runs"`
	FindingsProduced int     `json:"findings_produced"`
	FindingsKept     int     `json:"findings_kept"`
	Published        int     `json:"published"`
	Resolved         int     `json:"resolved"`
	Dismissed        int     `json:"dismissed"`
	MedianLatencyMS  int64   `json:"median_latency_ms"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	CostPerPublished float64 `json:"cost_per_published"`
}

func (s *Store) Stats() ([]AdapterStats, error) {
	rows, err := s.db.Query(`
		SELECT ar.adapter_id, ar.lens,
		       COUNT(DISTINCT ar.id),
		       COALESCE(SUM((SELECT COUNT(*) FROM findings f WHERE f.adapter_run_id = ar.id)), 0),
		       COALESCE(SUM((SELECT COUNT(*) FROM findings f WHERE f.adapter_run_id = ar.id AND f.kept = 1)), 0),
		       COALESCE(SUM((SELECT COUNT(*) FROM findings f
		            JOIN cluster_findings cf ON cf.finding_id = f.id
		            JOIN verdicts v ON v.cluster_id = cf.cluster_id
		            WHERE f.adapter_run_id = ar.id AND v.verdict = 'publish' AND v.posted_comment_id IS NOT NULL)), 0),
		       COALESCE(SUM((SELECT COUNT(*) FROM findings f
		            JOIN cluster_findings cf ON cf.finding_id = f.id
		            JOIN verdicts v ON v.cluster_id = cf.cluster_id
		            JOIN outcomes o ON o.verdict_id = v.id
		            WHERE f.adapter_run_id = ar.id AND o.resolved = 1)), 0),
		       COALESCE(SUM((SELECT COUNT(*) FROM findings f
		            JOIN cluster_findings cf ON cf.finding_id = f.id
		            JOIN verdicts v ON v.cluster_id = cf.cluster_id
		            JOIN outcomes o ON o.verdict_id = v.id
		            WHERE f.adapter_run_id = ar.id AND o.dismissed = 1)), 0),
		       COALESCE(SUM(ar.cost_usd), 0)
		FROM adapter_runs ar
		GROUP BY ar.adapter_id, ar.lens
		ORDER BY ar.adapter_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdapterStats
	for rows.Next() {
		var a AdapterStats
		if err := rows.Scan(&a.AdapterID, &a.Lens, &a.Runs, &a.FindingsProduced, &a.FindingsKept, &a.Published, &a.Resolved, &a.Dismissed, &a.TotalCostUSD); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].MedianLatencyMS = s.medianLatency(out[i].AdapterID)
		if out[i].Published > 0 {
			out[i].CostPerPublished = out[i].TotalCostUSD / float64(out[i].Published)
		}
	}
	return out, nil
}

func (s *Store) medianLatency(adapterID string) int64 {
	rows, err := s.db.Query(`SELECT duration_ms FROM adapter_runs WHERE adapter_id = ? ORDER BY duration_ms`, adapterID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var ds []int64
	for rows.Next() {
		var d int64
		if rows.Scan(&d) == nil {
			ds = append(ds, d)
		}
	}
	if len(ds) == 0 {
		return 0
	}
	return ds[len(ds)/2]
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
