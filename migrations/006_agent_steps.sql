CREATE TABLE agent_steps (
  unit_id TEXT NOT NULL REFERENCES review_units(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  round INTEGER NOT NULL CHECK (round > 0),
  model_call_id TEXT NOT NULL,
  prompt TEXT NOT NULL,
  response TEXT NOT NULL,
  tool_calls_json TEXT NOT NULL,
  tool_results_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (unit_id, round),
  UNIQUE (model_call_id)
);
