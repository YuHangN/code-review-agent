package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/report"
	"github.com/YuHangN/code-review-agent/internal/review"
	"github.com/YuHangN/code-review-agent/internal/verifier"
	"github.com/YuHangN/code-review-agent/internal/workflow"
)

func TestWorkflowContinuesPlannedRunThroughReport(t *testing.T) {
	store := newWorkflowStore(domain.RunStatusPlanned, []domain.ReviewUnit{
		{ID: "completed", UnitKey: "0", Risk: "high", Status: domain.UnitStatusCompleted},
		{ID: "medium", UnitKey: "b", Risk: "medium", Status: domain.UnitStatusPending},
		{ID: "high-b", UnitKey: "b", Risk: "high", Status: domain.UnitStatusFailedRecoverable},
		{ID: "high-a", UnitKey: "a", Risk: "high", Status: domain.UnitStatusRunning},
		{ID: "low", UnitKey: "a", Risk: "low", Status: domain.UnitStatusPending},
	})
	processor := &workflowProcessor{events: &store.events}
	aggregator := &workflowAggregator{events: &store.events}
	reporter := &workflowReporter{store: store, events: &store.events}
	flow := workflow.New(store, processor, aggregator, reporter)

	result, err := flow.Execute(context.Background(), executeRequest())
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"claim", "process:high-a", "process:high-b", "process:medium", "process:low",
		"aggregating", "release", "aggregate", "report",
	}
	if !reflect.DeepEqual(store.events, wantEvents) {
		t.Fatalf("events = %v, want %v", store.events, wantEvents)
	}
	if result.Status != domain.RunStatusReported || result.Units.Completed != 4 {
		t.Fatalf("result = %#v", result)
	}
	if result.Report.Report.OutputPath != "out/report.md" {
		t.Fatalf("report = %#v", result.Report)
	}
}

func TestWorkflowKeepsReviewingWhenAUnitFailsRecoverably(t *testing.T) {
	reviewErr := errors.New("model unavailable")
	store := newWorkflowStore(domain.RunStatusPlanned, []domain.ReviewUnit{
		{ID: "high", UnitKey: "a", Risk: "high", Status: domain.UnitStatusPending},
		{ID: "low", UnitKey: "b", Risk: "low", Status: domain.UnitStatusPending},
	})
	processor := &workflowProcessor{
		events:       &store.events,
		errorsByUnit: map[string]error{"high": reviewErr},
	}
	flow := workflow.New(store, processor, &workflowAggregator{}, &workflowReporter{})

	result, err := flow.Execute(context.Background(), executeRequest())
	if !errors.Is(err, reviewErr) {
		t.Fatalf("error = %v, want %v", err, reviewErr)
	}
	if result.Units.Completed != 1 || result.Units.FailedRecoverable != 1 {
		t.Fatalf("unit summary = %+v", result.Units)
	}
	wantEvents := []string{"claim", "process:high", "process:low", "release"}
	if !reflect.DeepEqual(store.events, wantEvents) {
		t.Fatalf("events = %v, want %v", store.events, wantEvents)
	}
	if store.run.Status != domain.RunStatusReviewing {
		t.Fatalf("run status = %s", store.run.Status)
	}
}

func TestWorkflowResumesFromAggregatingWithoutProcessingUnits(t *testing.T) {
	store := newWorkflowStore(domain.RunStatusAggregating, nil)
	flow := workflow.New(
		store,
		&workflowProcessor{events: &store.events},
		&workflowAggregator{events: &store.events},
		&workflowReporter{store: store, events: &store.events},
	)

	if _, err := flow.Execute(context.Background(), executeRequest()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.events, []string{"aggregate", "report"}) {
		t.Fatalf("events = %v", store.events)
	}
}

func TestWorkflowRestoresReportedRunWithoutEarlierStages(t *testing.T) {
	store := newWorkflowStore(domain.RunStatusReported, nil)
	reporter := &workflowReporter{store: store, events: &store.events, reused: true}
	flow := workflow.New(store, &workflowProcessor{}, &workflowAggregator{}, reporter)

	result, err := flow.Execute(context.Background(), executeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.events, []string{"report"}) || !result.Report.Reused {
		t.Fatalf("events = %v, result = %#v", store.events, result)
	}
}

func TestWorkflowReleasesLeaseWhenLoadingUnitsFails(t *testing.T) {
	listErr := errors.New("database unavailable")
	store := newWorkflowStore(domain.RunStatusPlanned, nil)
	store.listErr = listErr
	flow := workflow.New(store, &workflowProcessor{}, &workflowAggregator{}, &workflowReporter{})

	_, err := flow.Execute(context.Background(), executeRequest())
	if !errors.Is(err, listErr) {
		t.Fatalf("error = %v, want %v", err, listErr)
	}
	if !reflect.DeepEqual(store.events, []string{"claim", "release"}) {
		t.Fatalf("events = %v", store.events)
	}
}

func TestWorkflowRenewsLeaseWhileAUnitIsRunning(t *testing.T) {
	store := newWorkflowStore(domain.RunStatusPlanned, []domain.ReviewUnit{
		{ID: "slow", UnitKey: "a", Risk: "high", Status: domain.UnitStatusPending},
	})
	store.renewed = make(chan struct{})
	processor := &workflowProcessor{wait: store.renewed}
	flow := workflow.New(store, processor, &workflowAggregator{}, &workflowReporter{store: store})
	request := executeRequest()
	request.Lease = workflow.LeaseSettings{TTL: 100 * time.Millisecond, RenewInterval: time.Millisecond}

	if _, err := flow.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if store.claims.Load() < 2 {
		t.Fatalf("claim calls = %d, want initial claim and at least one renewal", store.claims.Load())
	}
}

func TestWorkflowStopsWhenLeaseRenewalFails(t *testing.T) {
	renewErr := errors.New("lease taken over")
	store := newWorkflowStore(domain.RunStatusPlanned, []domain.ReviewUnit{
		{ID: "slow", UnitKey: "a", Risk: "high", Status: domain.UnitStatusPending},
	})
	store.renewed = make(chan struct{})
	store.renewErr = renewErr
	processor := &workflowProcessor{wait: store.renewed}
	flow := workflow.New(store, processor, &workflowAggregator{}, &workflowReporter{})
	request := executeRequest()
	request.Lease = workflow.LeaseSettings{TTL: 100 * time.Millisecond, RenewInterval: time.Millisecond}

	_, err := flow.Execute(context.Background(), request)
	if !errors.Is(err, renewErr) {
		t.Fatalf("error = %v, want %v", err, renewErr)
	}
	if store.run.Status != domain.RunStatusReviewing {
		t.Fatalf("run status = %s", store.run.Status)
	}
}

func executeRequest() workflow.ExecuteRequest {
	return workflow.ExecuteRequest{
		RunID:      "run-001",
		Owner:      "worker-a",
		OutputPath: "out/report.md",
		Lease: workflow.LeaseSettings{
			TTL:           time.Minute,
			RenewInterval: 10 * time.Second,
		},
	}
}

type workflowStore struct {
	run      domain.Run
	units    []domain.ReviewUnit
	events   []string
	listErr  error
	claims   atomic.Int32
	renewed  chan struct{}
	renew    sync.Once
	renewErr error
}

func newWorkflowStore(status domain.RunStatus, units []domain.ReviewUnit) *workflowStore {
	return &workflowStore{run: domain.Run{ID: "run-001", Status: status}, units: units}
}

func (store *workflowStore) GetRun(context.Context, string) (domain.Run, error) {
	return store.run, nil
}

func (store *workflowStore) ClaimRun(_ context.Context, _ string, _ string, _ time.Time, _ time.Duration) (domain.Run, error) {
	if store.claims.Add(1) > 1 {
		store.renew.Do(func() { close(store.renewed) })
		return store.run, store.renewErr
	}
	store.events = append(store.events, "claim")
	store.run.Status = domain.RunStatusReviewing
	return store.run, nil
}

func (store *workflowStore) ListUnits(context.Context, string) ([]domain.ReviewUnit, error) {
	return append([]domain.ReviewUnit(nil), store.units...), store.listErr
}

func (store *workflowStore) AdvanceRunToAggregating(context.Context, string, string, time.Time) error {
	store.events = append(store.events, "aggregating")
	store.run.Status = domain.RunStatusAggregating
	return nil
}

func (store *workflowStore) ReleaseRunLease(context.Context, string, string) error {
	store.events = append(store.events, "release")
	return nil
}

type workflowProcessor struct {
	events       *[]string
	errorsByUnit map[string]error
	wait         <-chan struct{}
}

func (processor *workflowProcessor) Process(_ context.Context, unitID string, _ time.Time) (review.UnitOutcome, error) {
	if processor.wait != nil {
		<-processor.wait
	}
	if processor.events != nil {
		*processor.events = append(*processor.events, "process:"+unitID)
	}
	if err := processor.errorsByUnit[unitID]; err != nil {
		return review.UnitOutcome{UnitID: unitID, Status: domain.UnitStatusFailedRecoverable}, err
	}
	return review.UnitOutcome{UnitID: unitID, Status: domain.UnitStatusCompleted}, nil
}

type workflowAggregator struct {
	events *[]string
}

func (aggregator *workflowAggregator) Aggregate(context.Context, string, time.Time) (verifier.AggregatorResult, error) {
	if aggregator.events != nil {
		*aggregator.events = append(*aggregator.events, "aggregate")
	}
	return verifier.AggregatorResult{Candidates: 1, Findings: 1, Advisory: 1}, nil
}

type workflowReporter struct {
	store  *workflowStore
	events *[]string
	reused bool
}

func (reporter *workflowReporter) Generate(_ context.Context, request report.GenerateRequest) (report.GenerateResult, error) {
	if reporter.events != nil {
		*reporter.events = append(*reporter.events, "report")
	}
	if reporter.store != nil {
		reporter.store.run.Status = domain.RunStatusReported
	}
	return report.GenerateResult{
		Report: domain.Report{RunID: request.RunID, OutputPath: request.OutputPath},
		Reused: reporter.reused,
	}, nil
}
