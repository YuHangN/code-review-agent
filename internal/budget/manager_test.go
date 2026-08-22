package budget_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

func TestReserveEnforcesRunHardLimit(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	if err := manager.Reserve(ctx, reservation("call-1", 60)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reserve(ctx, reservation("call-2", 50)); !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("second reservation error = %v, want ErrLimitExceeded", err)
	}
	summary, err := manager.Summary(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ReservedMicros != 60 || summary.ActualMicros != 0 || summary.CommittedMicros != 60 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestSettleChargesActualAndReturnsUnusedReservation(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	if err := manager.Reserve(ctx, reservation("call-1", 60)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(ctx, "call-1", budget.Usage{ActualMicros: 30, InputTokens: 100, OutputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reserve(ctx, reservation("call-2", 70)); err != nil {
		t.Fatalf("reserve after settlement: %v", err)
	}
	summary, err := manager.Summary(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if summary.ActualMicros != 30 || summary.ReservedMicros != 70 || summary.CommittedMicros != 100 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestReleaseReturnsFailedCallReservation(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	if err := manager.Reserve(ctx, reservation("call-1", 100)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, "call-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reserve(ctx, reservation("call-2", 100)); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}
}

func TestReserveIsIdempotentForSameReservation(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	request := reservation("call-1", 60)
	if err := manager.Reserve(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reserve(ctx, request); err != nil {
		t.Fatalf("retry reservation: %v", err)
	}
	summary, err := manager.Summary(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if summary.CommittedMicros != 60 {
		t.Fatalf("committed micros = %d, want 60", summary.CommittedMicros)
	}
}

func TestReserveRejectsReuseOfReleasedReservationID(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	request := reservation("call-1", 60)
	if err := manager.Reserve(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reserve(ctx, request); !errors.Is(err, budget.ErrReservationConflict) {
		t.Fatalf("reuse released reservation error = %v, want ErrReservationConflict", err)
	}
}

func TestSettleIsIdempotentForSameUsage(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	if err := manager.Reserve(ctx, reservation("call-1", 60)); err != nil {
		t.Fatal(err)
	}
	usage := budget.Usage{ActualMicros: 30, InputTokens: 100, OutputTokens: 20}
	if err := manager.Settle(ctx, "call-1", usage); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(ctx, "call-1", usage); err != nil {
		t.Fatalf("retry settlement: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	if err := manager.Reserve(ctx, reservation("call-1", 60)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, "call-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, "call-1"); err != nil {
		t.Fatalf("retry release: %v", err)
	}
}

func TestConcurrentReservationsCannotExceedRunLimit(t *testing.T) {
	manager := newManager(t, 100)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, id := range []string{"call-1", "call-2"} {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			<-start
			results <- manager.Reserve(ctx, reservation(id, 60))
		}(id)
	}
	close(start)
	workers.Wait()
	close(results)

	var succeeded, rejected int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, budget.ErrLimitExceeded):
			rejected++
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("succeeded = %d, rejected = %d, want 1 and 1", succeeded, rejected)
	}
	summary, err := manager.Summary(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if summary.CommittedMicros != 60 {
		t.Fatalf("committed micros = %d, want 60", summary.CommittedMicros)
	}
}

func newManager(t *testing.T, limit int64) budget.Manager {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	unit := domain.ReviewUnit{ID: "unit-001", RunID: "run-001", UnitKey: "unit-key", FilePath: "main.go", Risk: "high", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, domain.Run{ID: "run-001", SourceURL: "https://example.test/pr/1", Provider: "fake", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: limit, CreatedAt: now, UpdatedAt: now}, []domain.ReviewUnit{unit}); err != nil {
		t.Fatal(err)
	}
	return budget.NewManager(store)
}

func reservation(id string, micros int64) budget.Reservation {
	return budget.Reservation{ID: id, RunID: "run-001", UnitID: "unit-001", Tier: "strong", ReservedMicros: micros, CreatedAt: time.Date(2026, 8, 22, 0, 1, 0, 0, time.UTC)}
}
