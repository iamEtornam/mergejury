CREATE TABLE runs (
    id                  INTEGER PRIMARY KEY,
    repo                TEXT NOT NULL DEFAULT '',
    pr_number           INTEGER NOT NULL DEFAULT 0,
    head_sha            TEXT NOT NULL DEFAULT '',
    base_sha            TEXT NOT NULL DEFAULT '',
    trigger             TEXT NOT NULL DEFAULT 'cli',
    status              TEXT NOT NULL DEFAULT 'running',  -- running | completed | degraded | failed | gated
    review_event        TEXT NOT NULL DEFAULT '',          -- APPROVE | REQUEST_CHANGES | COMMENT | '' (dry run / local)
    review_event_reason TEXT NOT NULL DEFAULT '',
    started_at          TEXT NOT NULL,
    finished_at         TEXT,
    total_cost_usd      REAL NOT NULL DEFAULT 0,
    config_snapshot     TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE adapter_runs (
    id            INTEGER PRIMARY KEY,
    run_id        INTEGER NOT NULL REFERENCES runs(id),
    adapter_id    TEXT NOT NULL,
    lens          TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT '',  -- ok | timeout | parse_error | auth_error | denied | crashed
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    cost_usd      REAL NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    raw_output    TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_adapter_runs_run ON adapter_runs(run_id);

CREATE TABLE findings (
    id              INTEGER PRIMARY KEY,
    adapter_run_id  INTEGER NOT NULL REFERENCES adapter_runs(id),
    path            TEXT NOT NULL,
    line            INTEGER NOT NULL,
    start_line      INTEGER,
    category        TEXT NOT NULL,
    severity        TEXT NOT NULL,
    title           TEXT NOT NULL,
    body            TEXT NOT NULL DEFAULT '',
    suggested_patch TEXT,
    evidence_json   TEXT NOT NULL DEFAULT '[]',
    confidence      TEXT NOT NULL DEFAULT '',
    kept            INTEGER NOT NULL DEFAULT 1,
    drop_reason     TEXT NOT NULL DEFAULT ''   -- schema | unanchored | bad_evidence | multiline | dupe | ''
);
CREATE INDEX idx_findings_adapter_run ON findings(adapter_run_id);

CREATE TABLE clusters (
    id              INTEGER PRIMARY KEY,
    run_id          INTEGER NOT NULL REFERENCES runs(id),
    path            TEXT NOT NULL,
    line            INTEGER NOT NULL,
    category        TEXT NOT NULL,
    supporter_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_clusters_run ON clusters(run_id);

CREATE TABLE cluster_findings (
    cluster_id INTEGER NOT NULL REFERENCES clusters(id),
    finding_id INTEGER NOT NULL REFERENCES findings(id),
    PRIMARY KEY (cluster_id, finding_id)
);

CREATE TABLE verifications (
    id         INTEGER PRIMARY KEY,
    cluster_id INTEGER NOT NULL REFERENCES clusters(id),
    kind       TEXT NOT NULL,
    command    TEXT NOT NULL DEFAULT '',
    exit_code  INTEGER NOT NULL DEFAULT 0,
    output     TEXT NOT NULL DEFAULT '',
    conclusion TEXT NOT NULL DEFAULT ''  -- supports | refutes | inconclusive
);

CREATE TABLE challenges (
    id          INTEGER PRIMARY KEY,
    cluster_id  INTEGER NOT NULL REFERENCES clusters(id),
    model       TEXT NOT NULL DEFAULT '',
    argument    TEXT NOT NULL DEFAULT '',
    could_argue INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE verdicts (
    id                INTEGER PRIMARY KEY,
    cluster_id        INTEGER NOT NULL REFERENCES clusters(id),
    verdict           TEXT NOT NULL,  -- publish | drop | needs_human
    reason            TEXT NOT NULL DEFAULT '',
    final_severity    TEXT NOT NULL DEFAULT '',
    final_body        TEXT NOT NULL DEFAULT '',
    final_patch       TEXT,
    posted_comment_id INTEGER,
    posted_at         TEXT
);

CREATE TABLE outcomes (
    id            INTEGER PRIMARY KEY,
    verdict_id    INTEGER NOT NULL REFERENCES verdicts(id),
    comment_state TEXT NOT NULL DEFAULT '',
    resolved      INTEGER NOT NULL DEFAULT 0,
    dismissed     INTEGER NOT NULL DEFAULT 0,
    reply_count   INTEGER NOT NULL DEFAULT 0,
    observed_at   TEXT NOT NULL
);

-- Idempotency: one posted review per (repo, pr, head_sha, event).
CREATE TABLE posted_reviews (
    id         INTEGER PRIMARY KEY,
    run_id     INTEGER NOT NULL REFERENCES runs(id),
    repo       TEXT NOT NULL,
    pr_number  INTEGER NOT NULL,
    head_sha   TEXT NOT NULL,
    event      TEXT NOT NULL,
    review_id  INTEGER NOT NULL,
    posted_at  TEXT NOT NULL
);
CREATE INDEX idx_posted_reviews_pr ON posted_reviews(repo, pr_number);
