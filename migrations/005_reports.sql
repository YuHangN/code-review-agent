CREATE TABLE reports (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  output_path TEXT NOT NULL,
  content TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  created_at TEXT NOT NULL
);
