package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

func TestOpenMigratesExistingReviewUnitsWithPersistedInputColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(ctx, `CREATE TABLE review_units (
id TEXT PRIMARY KEY, run_id TEXT NOT NULL, unit_key TEXT NOT NULL, file_path TEXT NOT NULL,
risk TEXT NOT NULL, status TEXT NOT NULL, attempt INTEGER NOT NULL, created_at TEXT NOT NULL,
updated_at TEXT NOT NULL, UNIQUE(run_id, unit_key))`)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := sqlite.Open(ctx, path, sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.QueryContext(ctx, `PRAGMA table_info(review_units)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]bool{"start_line": false, "end_line": false, "diff_hunk": false}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for column, found := range want {
		if !found {
			t.Fatalf("migration did not add review_units.%s", column)
		}
	}
}

func TestStoreCreateRunPersistsUnitsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")
	createdAt := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID:                "run-001",
		SourceURL:         "https://example.test/org/repository/changes/1",
		Provider:          "github",
		Repository:        "org/repository",
		ChangeNumber:      1,
		Status:            domain.RunStatusCreated,
		BudgetLimitMicros: 1_000_000,
		CreatedAt:         createdAt,
		UpdatedAt:         createdAt,
	}
	units := []domain.ReviewUnit{
		{
			ID:        "unit-001",
			RunID:     run.ID,
			UnitKey:   "src/main.go#1-10",
			FilePath:  "src/main.go",
			Risk:      "medium",
			Status:    domain.UnitStatusPending,
			Attempt:   0,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
		{
			ID:        "unit-002",
			RunID:     run.ID,
			UnitKey:   "src/main.go#11-20",
			FilePath:  "src/main.go",
			Risk:      "high",
			Status:    domain.UnitStatusCompleted,
			Attempt:   1,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
	}

	store, err := sqlite.Open(ctx, path, sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, run, units); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlite.Open(ctx, path, sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	gotRun, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != domain.RunStatusCreated {
		t.Fatalf("run status = %q, want %q", gotRun.Status, domain.RunStatusCreated)
	}

	gotUnits, err := store.ListUnits(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotUnits) != 2 {
		t.Fatalf("unit count = %d, want 2", len(gotUnits))
	}
	sort.Slice(gotUnits, func(i, j int) bool { return gotUnits[i].UnitKey < gotUnits[j].UnitKey })
	if gotUnits[0].UnitKey != "src/main.go#1-10" || gotUnits[1].UnitKey != "src/main.go#11-20" {
		t.Fatalf("unit keys = [%q, %q], want persisted keys", gotUnits[0].UnitKey, gotUnits[1].UnitKey)
	}
	if gotUnits[1].Status != domain.UnitStatusCompleted {
		t.Fatalf("completed unit status = %q, want %q", gotUnits[1].Status, domain.UnitStatusCompleted)
	}
}

func TestSaveFetchedSnapshotRejectsIncompleteSnapshotWithoutAdvancingRun(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 23, 4, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-fetching", SourceURL: "https://github.com/acme/repo/pull/1",
		Provider: "github", Repository: "acme/repo", ChangeNumber: 1,
		Status: domain.RunStatusCreated, BudgetLimitMicros: 1_000_000,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginFetch(ctx, run.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	err := store.SaveFetchedSnapshot(ctx, run.ID, domain.ChangeSnapshot{BaseSHA: "base-sha"}, now.Add(2*time.Second))
	if err == nil {
		t.Fatal("incomplete snapshot was accepted")
	}
	storedRun, getErr := store.GetRun(ctx, run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if storedRun.Status != domain.RunStatusFetching {
		t.Fatalf("run status = %q, want %q", storedRun.Status, domain.RunStatusFetching)
	}
	if _, getErr := store.GetSnapshot(ctx, run.ID); getErr == nil {
		t.Fatal("incomplete snapshot was persisted")
	}
}

func TestStoreClaimRunRejectsOtherOwnerUntilLeaseExpires(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.CreateRun(ctx, domain.Run{
		ID:                "run-001",
		SourceURL:         "https://example.test/org/repository/changes/1",
		Provider:          "github",
		Repository:        "org/repository",
		ChangeNumber:      1,
		Status:            domain.RunStatusCreated,
		BudgetLimitMicros: 1_000_000,
		CreatedAt:         time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
	}, nil); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	if _, err := store.ClaimRun(ctx, "run-001", "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, "run-001", "worker-b", now.Add(30*time.Second), time.Minute); !errors.Is(err, sqlite.ErrLeaseHeld) {
		t.Fatalf("claim while leased error = %v, want ErrLeaseHeld", err)
	}
	claimed, err := store.ClaimRun(ctx, "run-001", "worker-b", now.Add(61*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.LeaseOwner != "worker-b" {
		t.Fatalf("lease owner = %q, want %q", claimed.LeaseOwner, "worker-b")
	}
}

func TestStoreReleaseRunLeaseRequiresCurrentOwner(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "run-release", SourceURL: "https://example.test/pr/1", Provider: "fake", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, run.ID, "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseRunLease(ctx, run.ID, "worker-b"); !errors.Is(err, sqlite.ErrLeaseHeld) {
		t.Fatalf("release by other owner error = %v, want ErrLeaseHeld", err)
	}
	if err := store.ReleaseRunLease(ctx, run.ID, "worker-a"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseOwner != "" || !got.LeaseExpiresAt.IsZero() {
		t.Fatalf("lease after release = owner %q, expiry %v", got.LeaseOwner, got.LeaseExpiresAt)
	}
}

func TestAdvanceRunToAggregatingRequiresTerminalUnitsAndCurrentLease(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-aggregate", SourceURL: "https://example.test/pr/1", Provider: "fake",
		Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusReviewing,
		BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now,
	}
	units := []domain.ReviewUnit{
		{ID: "unit-completed", RunID: run.ID, UnitKey: "a", Status: domain.UnitStatusCompleted, CreatedAt: now, UpdatedAt: now},
		{ID: "unit-pending", RunID: run.ID, UnitKey: "b", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now},
	}
	if err := store.CreateRun(ctx, run, units); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, run.ID, "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := store.AdvanceRunToAggregating(ctx, run.ID, "worker-a", now.Add(time.Second)); !errors.Is(err, sqlite.ErrRunNotReady) {
		t.Fatalf("advance with pending unit error = %v, want ErrRunNotReady", err)
	}

	// 创建一个只有终态 Unit 的 Run，验证成功推进和 owner 校验。
	terminalRun := run
	terminalRun.ID = "run-terminal"
	terminalUnits := []domain.ReviewUnit{
		{ID: "unit-terminal-a", RunID: terminalRun.ID, UnitKey: "a", Status: domain.UnitStatusCompleted, CreatedAt: now, UpdatedAt: now},
		{ID: "unit-terminal-b", RunID: terminalRun.ID, UnitKey: "b", Status: domain.UnitStatusSkippedBudget, CreatedAt: now, UpdatedAt: now},
	}
	if err := store.CreateRun(ctx, terminalRun, terminalUnits); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, terminalRun.ID, "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceRunToAggregating(ctx, terminalRun.ID, "worker-b", now.Add(time.Second)); !errors.Is(err, sqlite.ErrLeaseHeld) {
		t.Fatalf("advance by other owner error = %v, want ErrLeaseHeld", err)
	}
	if err := store.AdvanceRunToAggregating(ctx, terminalRun.ID, "worker-a", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRun(ctx, terminalRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.RunStatusAggregating {
		t.Fatalf("run status = %q, want %q", got.Status, domain.RunStatusAggregating)
	}
}

func TestReplaceVerifiedFindingsIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "run-verified", SourceURL: "https://example.test/pr/1", Provider: "fake", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	unit := domain.ReviewUnit{ID: "unit-verified", RunID: run.ID, UnitKey: "a", FilePath: "config.yaml", DiffHunk: "@@ -0,0 +1 @@\n+api_key: value\n", Risk: "high", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, []domain.ReviewUnit{unit}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, run.ID, "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartReviewUnit(ctx, unit.ID, "worker-a", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	trace := domain.ReviewTrace{ID: "trace-verified", RunID: run.ID, UnitID: unit.ID, CallID: "call-verified", Detector: "llm_review", Status: string(domain.UnitStatusCompleted), Prompt: "prompt", Response: "response", CreatedAt: now}
	candidate := domain.CandidateFindingRecord{ID: "candidate-verified", RunID: run.ID, UnitID: unit.ID, TraceID: trace.ID, Detector: "llm_review", Category: "security", Severity: "high", File: "config.yaml", Line: 1, Title: "硬编码 secret", Explanation: "说明", Evidence: []string{"证据"}, Suggestion: "修改", CreatedAt: now}
	if err := store.CompleteReviewUnit(ctx, trace, []domain.CandidateFindingRecord{candidate}, "worker-a", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceRunToAggregating(ctx, run.ID, "worker-a", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	finding := domain.VerifiedFinding{
		ID: "finding-verified", RunID: run.ID, CandidateID: candidate.ID, TraceID: trace.ID,
		Fingerprint: "fingerprint-verified", Confidence: domain.ConfidenceConfirmed,
		VerificationSource: "rule:test", VerificationReason: "确定性规则命中",
		Category: candidate.Category, Severity: candidate.Severity, File: candidate.File, Line: candidate.Line,
		Title: candidate.Title, Explanation: candidate.Explanation, Evidence: []string{"规则证据"}, Suggestion: candidate.Suggestion, CreatedAt: now,
	}

	if err := store.ReplaceVerifiedFindings(ctx, run.ID, []domain.VerifiedFinding{finding}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceVerifiedFindings(ctx, run.ID, []domain.VerifiedFinding{finding}); err != nil {
		t.Fatalf("idempotent replacement failed: %v", err)
	}
	findings, err := store.ListVerifiedFindings(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].ID != finding.ID || findings[0].Confidence != domain.ConfidenceConfirmed || len(findings[0].Evidence) != 1 {
		t.Fatalf("verified findings = %#v", findings)
	}
}

func TestSaveReportCheckpointsContentAndMarksRunReported(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "run-report", SourceURL: "https://example.test/pr/1", Provider: "fake", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusAggregating, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	report := domain.Report{RunID: run.ID, OutputPath: "out/report.md", Content: "# Report\n", ContentSHA256: "sha256-value", CreatedAt: now}

	if err := store.SaveReport(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveReport(ctx, report, now.Add(time.Second)); err != nil {
		t.Fatalf("idempotent report checkpoint failed: %v", err)
	}
	got, err := store.GetReport(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != report {
		t.Fatalf("stored report = %#v, want %#v", got, report)
	}
	gotRun, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != domain.RunStatusReported {
		t.Fatalf("run status = %q, want %q", gotRun.Status, domain.RunStatusReported)
	}
}

func TestAgentStepCheckpointIsOrderedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "run-agent", SourceURL: "https://example.test/pr/1", Provider: "github", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	unit := domain.ReviewUnit{ID: "unit-agent", RunID: run.ID, UnitKey: "main.go#1", FilePath: "main.go", Risk: "high", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, []domain.ReviewUnit{unit}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, run.ID, "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	step := domain.AgentStep{
		RunID: run.ID, UnitID: unit.ID, Round: 1, ModelCallID: "call-unit-agent-1-round-1",
		Prompt: "prompt", Response: `{"tool_calls":[]}`,
		ToolCalls:   []domain.AgentToolCall{{ID: "tool-1", Name: "read_file", Arguments: `{"path":"main.go"}`}},
		ToolResults: []domain.AgentToolResult{{CallID: "tool-1", Name: "read_file", Content: `{"content":"package main"}`}},
		CreatedAt:   now,
	}
	if err := store.SaveAgentStep(ctx, step, "worker-a", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentStep(ctx, step, "worker-a", now.Add(2*time.Second)); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	steps, err := store.ListAgentSteps(ctx, unit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].ModelCallID != step.ModelCallID || len(steps[0].ToolResults) != 1 || steps[0].ToolResults[0].Content != step.ToolResults[0].Content {
		t.Fatalf("agent steps = %#v", steps)
	}
	conflict := step
	conflict.Response = `{"findings":[]}`
	if err := store.SaveAgentStep(ctx, conflict, "worker-a", now.Add(3*time.Second)); !errors.Is(err, sqlite.ErrAgentStepConflict) {
		t.Fatalf("conflicting save error = %v", err)
	}
}

func TestAgentStepCheckpointRejectsProcessWithoutRunLease(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "run-agent-lease", SourceURL: "https://example.test/pr/1", Provider: "github", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	unit := domain.ReviewUnit{ID: "unit-agent-lease", RunID: run.ID, UnitKey: "main.go#1", Status: domain.UnitStatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, []domain.ReviewUnit{unit}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, run.ID, "worker-new", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	step := domain.AgentStep{RunID: run.ID, UnitID: unit.ID, Round: 1, ModelCallID: "call-1", Prompt: "prompt", Response: `{"findings":[]}`, CreatedAt: now}

	if err := store.SaveAgentStep(ctx, step, "worker-old", now.Add(time.Second)); !errors.Is(err, sqlite.ErrLeaseHeld) {
		t.Fatalf("old owner save error = %v", err)
	}
	steps, err := store.ListAgentSteps(ctx, unit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("old owner persisted steps = %#v", steps)
	}
}

func TestAgentStepCheckpointRejectsUnitFromDifferentRun(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	runA := domain.Run{ID: "run-a", SourceURL: "https://example.test/pr/1", Provider: "github", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	runB := runA
	runB.ID = "run-b"
	unitB := domain.ReviewUnit{ID: "unit-b", RunID: runB.ID, UnitKey: "main.go#1", Status: domain.UnitStatusRunning, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, runA, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, runB, []domain.ReviewUnit{unitB}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, runA.ID, "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	step := domain.AgentStep{RunID: runA.ID, UnitID: unitB.ID, Round: 1, ModelCallID: "call-1", Prompt: "prompt", Response: `{"findings":[]}`, CreatedAt: now}

	if err := store.SaveAgentStep(ctx, step, "worker-a", now.Add(time.Second)); err == nil {
		t.Fatal("cross-run agent step was accepted")
	}
	steps, err := store.ListAgentSteps(ctx, unitB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("cross-run steps = %#v", steps)
	}
}

func TestStoreClaimRunRejectsEmptyOwner(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.CreateRun(ctx, domain.Run{
		ID:                "run-001",
		SourceURL:         "https://example.test/org/repository/changes/1",
		Provider:          "github",
		Repository:        "org/repository",
		ChangeNumber:      1,
		Status:            domain.RunStatusCreated,
		BudgetLimitMicros: 1_000_000,
		CreatedAt:         time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
	}, nil); err != nil {
		t.Fatal(err)
	}

	_, err = store.ClaimRun(ctx, "run-001", "", time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC), time.Minute)
	if !errors.Is(err, sqlite.ErrLeaseOwnerRequired) {
		t.Fatalf("ClaimRun with empty owner error = %v, want ErrLeaseOwnerRequired", err)
	}
}

func TestStorePersistsImmutableSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	run := domain.Run{
		ID:                "run-001",
		SourceURL:         "https://github.com/acme/payments/pull/42",
		Provider:          "github",
		Repository:        "acme/payments",
		ChangeNumber:      42,
		Status:            domain.RunStatusFetched,
		BudgetLimitMicros: 1_000_000,
		CreatedAt:         time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
	}
	if err := store.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.ChangeSnapshot{
		BaseSHA:    "base-sha",
		HeadSHA:    "head-sha",
		Diff:       "diff --git a/file.go b/file.go\n",
		DiffSHA256: "hash",
		CreatedAt:  time.Date(2026, 8, 22, 0, 1, 0, 0, time.UTC),
	}
	if err := store.SaveSnapshot(ctx, run.ID, snapshot); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetSnapshot(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSHA != snapshot.BaseSHA || got.HeadSHA != snapshot.HeadSHA || got.Diff != snapshot.Diff || got.DiffSHA256 != snapshot.DiffSHA256 {
		t.Fatalf("snapshot = %#v, want %#v", got, snapshot)
	}
	if err := store.SaveSnapshot(ctx, run.ID, snapshot); err == nil {
		t.Fatal("saving a second snapshot succeeded, want immutable conflict")
	}
}

func TestStoreSavePlanPersistsUnitsAndPlannedStatus(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	run := fetchedRun("run-plan")
	if err := store.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	now := run.CreatedAt.Add(time.Minute)
	units := []domain.ReviewUnit{{
		ID: "unit-1", RunID: run.ID, UnitKey: "key-1", FilePath: "main.go",
		StartLine: 12, EndLine: 14, DiffHunk: "@@ -12 +12,3 @@\n+changed\n",
		Risk: "medium", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now,
	}}

	if err := store.SavePlan(ctx, run.ID, units, now); err != nil {
		t.Fatal(err)
	}
	gotRun, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotUnits, err := store.ListUnits(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != domain.RunStatusPlanned || len(gotUnits) != 1 || gotUnits[0].ID != "unit-1" {
		t.Fatalf("run status = %q, units = %#v", gotRun.Status, gotUnits)
	}
	if gotUnits[0].StartLine != 12 || gotUnits[0].EndLine != 14 || gotUnits[0].DiffHunk != units[0].DiffHunk {
		t.Fatalf("persisted unit input = %#v, want %#v", gotUnits[0], units[0])
	}
}

func TestStoreSavePlanRollsBackStatusWhenUnitInsertFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	run := fetchedRun("run-rollback")
	if err := store.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	now := run.CreatedAt.Add(time.Minute)
	units := []domain.ReviewUnit{
		{ID: "same-id", RunID: run.ID, UnitKey: "key-1", FilePath: "a.go", Risk: "medium", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now},
		{ID: "same-id", RunID: run.ID, UnitKey: "key-2", FilePath: "b.go", Risk: "medium", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now},
	}

	if err := store.SavePlan(ctx, run.ID, units, now); err == nil {
		t.Fatal("SavePlan succeeded, want duplicate unit failure")
	}
	gotRun, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotUnits, err := store.ListUnits(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != domain.RunStatusFetched || len(gotUnits) != 0 {
		t.Fatalf("rollback left status = %q, units = %#v", gotRun.Status, gotUnits)
	}
}

func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func fetchedRun(id string) domain.Run {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	return domain.Run{ID: id, SourceURL: "https://github.com/acme/repo/pull/1", Provider: "github", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusFetched, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
}
