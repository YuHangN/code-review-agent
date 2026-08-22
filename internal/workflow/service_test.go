package workflow_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
	"github.com/YuHangN/code-review-agent/internal/workflow"
)

func TestStartPersistsRunAndUnits(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	service := workflow.NewService(store)
	run := testRun("run-001")
	units := []domain.ReviewUnit{testUnit(run.ID, "unit-001", domain.UnitStatusPending)}

	if err := service.Start(ctx, workflow.StartRequest{Run: run, Units: units}); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != run.ID {
		t.Fatalf("stored run ID = %q, want %q", got.ID, run.ID)
	}
	gotUnits, err := store.ListUnits(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotUnits) != 1 || gotUnits[0].ID != "unit-001" {
		t.Fatalf("stored units = %#v, want unit-001", gotUnits)
	}
}

func TestStartRejectsUnitForDifferentRun(t *testing.T) {
	store := openStore(t)
	service := workflow.NewService(store)
	run := testRun("run-001")

	err := service.Start(context.Background(), workflow.StartRequest{
		Run:   run,
		Units: []domain.ReviewUnit{testUnit("run-002", "unit-001", domain.UnitStatusPending)},
	})
	if !errors.Is(err, workflow.ErrUnitRunMismatch) {
		t.Fatalf("Start error = %v, want ErrUnitRunMismatch", err)
	}
}

func TestResumeReturnsOnlyResumableUnits(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	run := testRun("run-001")
	units := []domain.ReviewUnit{
		testUnit(run.ID, "pending", domain.UnitStatusPending),
		testUnit(run.ID, "running", domain.UnitStatusRunning),
		testUnit(run.ID, "retry", domain.UnitStatusFailedRecoverable),
		testUnit(run.ID, "completed", domain.UnitStatusCompleted),
		testUnit(run.ID, "budget", domain.UnitStatusSkippedBudget),
	}
	if err := store.CreateRun(ctx, run, units); err != nil {
		t.Fatal(err)
	}

	result, err := workflow.NewService(store).Resume(
		ctx,
		run.ID,
		"worker-a",
		time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(result.PendingUnits))
	for _, unit := range result.PendingUnits {
		got = append(got, unit.ID)
	}
	want := []string{"budget", "completed", "pending", "retry", "running"}
	if len(got) != 3 || got[0] != "pending" || got[1] != "retry" || got[2] != "running" {
		t.Fatalf("resumable unit IDs = %v, want [pending retry running]; all IDs = %v", got, want)
	}
}

func TestResumeReturnsLeaseConflict(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	run := testRun("run-001")
	if err := store.CreateRun(ctx, run, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	if _, err := store.ClaimRun(ctx, run.ID, "worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	_, err := workflow.NewService(store).Resume(ctx, run.ID, "worker-b", now.Add(30*time.Second), time.Minute)
	if !errors.Is(err, sqlite.ErrLeaseHeld) {
		t.Fatalf("Resume error = %v, want ErrLeaseHeld", err)
	}
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testRun(id string) domain.Run {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	return domain.Run{
		ID:                id,
		SourceURL:         "https://example.test/acme/demo/changes/42",
		Provider:          "fake",
		Repository:        "acme/demo",
		ChangeNumber:      42,
		Status:            domain.RunStatusCreated,
		BudgetLimitMicros: 1_000_000,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

func testUnit(runID, id string, status domain.UnitStatus) domain.ReviewUnit {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	return domain.ReviewUnit{
		ID:        id,
		RunID:     runID,
		UnitKey:   id,
		FilePath:  "internal/example.go",
		Risk:      "medium",
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
}
