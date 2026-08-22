package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

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
	units := []domain.ReviewUnit{{ID: "unit-1", RunID: run.ID, UnitKey: "key-1", FilePath: "main.go", Risk: "medium", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now}}

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
