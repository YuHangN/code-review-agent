CREATE TABLE checker_runs (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  checker TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'completed', 'failed_recoverable')),
  attempt INTEGER NOT NULL DEFAULT 0,
  command_json TEXT NOT NULL DEFAULT '[]',
  exit_code INTEGER NOT NULL DEFAULT 0,
  output TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(run_id, checker)
);

CREATE TABLE checker_diagnostics (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  checker_run_id TEXT NOT NULL REFERENCES checker_runs(id) ON DELETE CASCADE,
  unit_id TEXT NOT NULL REFERENCES review_units(id) ON DELETE CASCADE,
  trace_id TEXT NOT NULL REFERENCES review_traces(id) ON DELETE CASCADE,
  checker TEXT NOT NULL,
  file_path TEXT NOT NULL,
  line INTEGER NOT NULL,
  column_number INTEGER NOT NULL,
  diagnostic_code TEXT NOT NULL,
  message TEXT NOT NULL,
  severity TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX checker_diagnostics_run_idx ON checker_diagnostics(run_id);

CREATE TABLE verified_findings_v2 (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  candidate_id TEXT REFERENCES candidate_findings(id) ON DELETE CASCADE,
  trace_id TEXT NOT NULL REFERENCES review_traces(id) ON DELETE CASCADE,
  fingerprint TEXT NOT NULL,
  confidence TEXT NOT NULL CHECK (confidence IN ('confirmed', 'advisory')),
  verification_source TEXT NOT NULL,
  verification_reason TEXT NOT NULL,
  category TEXT NOT NULL,
  severity TEXT NOT NULL,
  file_path TEXT NOT NULL,
  line INTEGER NOT NULL,
  title TEXT NOT NULL,
  explanation TEXT NOT NULL,
  evidence_json TEXT NOT NULL,
  suggestion TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(run_id, fingerprint)
);

INSERT INTO verified_findings_v2 SELECT * FROM verified_findings;
DROP TABLE verified_findings;
ALTER TABLE verified_findings_v2 RENAME TO verified_findings;
