PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  source_url TEXT NOT NULL,
  provider TEXT NOT NULL,
  repository TEXT NOT NULL,
  change_number INTEGER NOT NULL,
  status TEXT NOT NULL,
  budget_limit_micros INTEGER NOT NULL,
  lease_owner TEXT,
  lease_expires_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS review_units (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  unit_key TEXT NOT NULL,
  file_path TEXT NOT NULL,
  risk TEXT NOT NULL,
  status TEXT NOT NULL,
  attempt INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(run_id, unit_key)
);

CREATE TABLE IF NOT EXISTS change_snapshots (
  run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
  base_sha TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  diff TEXT NOT NULL,
  diff_sha256 TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS budget_ledger (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  unit_id TEXT NOT NULL REFERENCES review_units(id) ON DELETE CASCADE,
  tier TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('reserved', 'settled', 'released')),
  reserved_micros INTEGER NOT NULL CHECK (reserved_micros > 0),
  actual_micros INTEGER NOT NULL DEFAULT 0 CHECK (actual_micros >= 0),
  input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  created_at TEXT NOT NULL
);
