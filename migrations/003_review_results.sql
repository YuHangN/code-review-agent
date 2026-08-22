CREATE TABLE review_traces (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  unit_id TEXT NOT NULL REFERENCES review_units(id) ON DELETE CASCADE,
  call_id TEXT NOT NULL UNIQUE,
  detector TEXT NOT NULL,
  status TEXT NOT NULL,
  prompt TEXT NOT NULL,
  response TEXT NOT NULL,
  rejections_json TEXT NOT NULL,
  error_message TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE candidate_findings (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  unit_id TEXT NOT NULL REFERENCES review_units(id) ON DELETE CASCADE,
  trace_id TEXT NOT NULL REFERENCES review_traces(id) ON DELETE CASCADE,
  detector TEXT NOT NULL,
  category TEXT NOT NULL,
  severity TEXT NOT NULL,
  file_path TEXT NOT NULL,
  line INTEGER NOT NULL,
  title TEXT NOT NULL,
  explanation TEXT NOT NULL,
  evidence_json TEXT NOT NULL,
  suggestion TEXT NOT NULL,
  created_at TEXT NOT NULL
);
