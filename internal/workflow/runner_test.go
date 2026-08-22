package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/review"
	"github.com/YuHangN/code-review-agent/internal/workflow"
)

func TestRunnerExecutesResumableUnitsByRiskAndAdvancesRun(t *testing.T) {
	coordinator := &runnerCoordinator{
		resumeResult: workflow.ResumeResult{
			Run: domain.Run{ID: "run-001"},
			PendingUnits: []domain.ReviewUnit{
				{ID: "unit-medium", UnitKey: "b", Risk: "medium", Status: domain.UnitStatusPending},
				{ID: "unit-high-b", UnitKey: "b", Risk: "high", Status: domain.UnitStatusFailedRecoverable},
				{ID: "unit-high-a", UnitKey: "a", Risk: "high", Status: domain.UnitStatusRunning},
				{ID: "unit-low", UnitKey: "a", Risk: "low", Status: domain.UnitStatusPending},
			},
		},
	}
	executor := &runnerExecutor{}
	runner := workflow.NewRunner(coordinator, executor)

	result, err := runner.Run(context.Background(), workflow.RunRequest{
		RunID: "run-001", Owner: "worker-a",
		Lease: workflow.LeaseSettings{TTL: time.Minute, RenewInterval: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantOrder := []string{"unit-high-a", "unit-high-b", "unit-medium", "unit-low"}
	if !reflect.DeepEqual(executor.unitIDs, wantOrder) {
		t.Fatalf("execution order = %v, want %v", executor.unitIDs, wantOrder)
	}
	if result.Completed != 4 || result.FailedRecoverable != 0 || result.SkippedBudget != 0 {
		t.Fatalf("result = %+v", result)
	}
	if coordinator.aggregatedRunID != "run-001" || coordinator.aggregatedOwner != "worker-a" {
		t.Fatalf("aggregating call = (%q, %q)", coordinator.aggregatedRunID, coordinator.aggregatedOwner)
	}
	if !coordinator.leaseStarted || !coordinator.leaseStopped {
		t.Fatalf("lease lifecycle: started=%v stopped=%v", coordinator.leaseStarted, coordinator.leaseStopped)
	}
	if !coordinator.leaseReleased {
		t.Fatal("run lease was not released")
	}
}

func TestRunnerKeepsRunReviewingWhenAUnitFailsRecoverably(t *testing.T) {
	reviewErr := errors.New("model unavailable")
	coordinator := &runnerCoordinator{resumeResult: workflow.ResumeResult{
		Run: domain.Run{ID: "run-001"},
		PendingUnits: []domain.ReviewUnit{
			{ID: "unit-high", UnitKey: "a", Risk: "high", Status: domain.UnitStatusPending},
			{ID: "unit-low", UnitKey: "b", Risk: "low", Status: domain.UnitStatusPending},
		},
	}}
	executor := &runnerExecutor{errorsByUnit: map[string]error{"unit-high": reviewErr}}
	runner := workflow.NewRunner(coordinator, executor)

	result, err := runner.Run(context.Background(), workflow.RunRequest{
		RunID: "run-001", Owner: "worker-a",
		Lease: workflow.LeaseSettings{TTL: time.Minute, RenewInterval: 10 * time.Second},
	})
	if !errors.Is(err, reviewErr) {
		t.Fatalf("error = %v, want %v", err, reviewErr)
	}
	if result.Completed != 1 || result.FailedRecoverable != 1 {
		t.Fatalf("result = %+v", result)
	}
	if coordinator.aggregatedRunID != "" {
		t.Fatalf("failed run advanced to aggregating: %q", coordinator.aggregatedRunID)
	}
	if !reflect.DeepEqual(executor.unitIDs, []string{"unit-high", "unit-low"}) {
		t.Fatalf("execution order = %v", executor.unitIDs)
	}
}

type runnerCoordinator struct {
	resumeResult    workflow.ResumeResult
	resumeErr       error
	leaseErrors     chan error
	leaseStarted    bool
	leaseStopped    bool
	leaseReleased   bool
	aggregatedRunID string
	aggregatedOwner string
}

func (c *runnerCoordinator) Resume(context.Context, string, string, time.Time, time.Duration) (workflow.ResumeResult, error) {
	return c.resumeResult, c.resumeErr
}

func (c *runnerCoordinator) MaintainLease(ctx context.Context, _ string, _ string, _ workflow.LeaseSettings) (<-chan error, error) {
	c.leaseStarted = true
	if c.leaseErrors == nil {
		c.leaseErrors = make(chan error)
	}
	go func() {
		<-ctx.Done()
		c.leaseStopped = true
		close(c.leaseErrors)
	}()
	return c.leaseErrors, nil
}

func (c *runnerCoordinator) AdvanceToAggregating(_ context.Context, runID, owner string, _ time.Time) error {
	c.aggregatedRunID = runID
	c.aggregatedOwner = owner
	return nil
}

func (c *runnerCoordinator) ReleaseLease(context.Context, string, string) error {
	c.leaseReleased = true
	return nil
}

type runnerExecutor struct {
	unitIDs      []string
	errorsByUnit map[string]error
}

func (e *runnerExecutor) Execute(_ context.Context, unitID string, _ time.Time) (review.ExecutionOutcome, error) {
	e.unitIDs = append(e.unitIDs, unitID)
	if err := e.errorsByUnit[unitID]; err != nil {
		return review.ExecutionOutcome{UnitID: unitID, Status: domain.UnitStatusFailedRecoverable}, err
	}
	return review.ExecutionOutcome{UnitID: unitID, Status: domain.UnitStatusCompleted}, nil
}
