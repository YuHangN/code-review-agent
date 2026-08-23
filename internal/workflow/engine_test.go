package workflow_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/report"
	"github.com/YuHangN/code-review-agent/internal/verifier"
	"github.com/YuHangN/code-review-agent/internal/workflow"
)

func TestExecutionEngineContinuesPlannedRunThroughReport(t *testing.T) {
	store := &engineStore{run: domain.Run{ID: "run-001", Status: domain.RunStatusPlanned}}
	runner := &engineRunner{store: store}
	aggregator := &engineAggregator{events: &runner.events}
	reporter := &engineReporter{store: store, events: &runner.events}
	engine := workflow.NewExecutionEngine(store, runner, aggregator, reporter)

	result, err := engine.Execute(context.Background(), workflow.EngineRequest{
		RunID: "run-001", Owner: "worker-a", OutputPath: "out/report.md",
		Lease: workflow.LeaseSettings{TTL: time.Minute, RenewInterval: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.events, []string{"runner", "aggregator", "reporter"}) {
		t.Fatalf("execution events = %v", runner.events)
	}
	if result.Status != domain.RunStatusReported || result.Report.Report.OutputPath != "out/report.md" {
		t.Fatalf("engine result = %#v", result)
	}
}

func TestExecutionEngineResumesFromAggregatingWithoutReviewer(t *testing.T) {
	store := &engineStore{run: domain.Run{ID: "run-001", Status: domain.RunStatusAggregating}}
	runner := &engineRunner{store: store}
	aggregator := &engineAggregator{events: &runner.events}
	reporter := &engineReporter{store: store, events: &runner.events}
	engine := workflow.NewExecutionEngine(store, runner, aggregator, reporter)

	_, err := engine.Execute(context.Background(), workflow.EngineRequest{
		RunID: "run-001", Owner: "worker-a", OutputPath: "out/report.md",
		Lease: workflow.LeaseSettings{TTL: time.Minute, RenewInterval: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.events, []string{"aggregator", "reporter"}) {
		t.Fatalf("execution events = %v", runner.events)
	}
}

func TestExecutionEngineRestoresReportedRunWithoutReexecutingEarlierStages(t *testing.T) {
	store := &engineStore{run: domain.Run{ID: "run-001", Status: domain.RunStatusReported}}
	runner := &engineRunner{store: store}
	aggregator := &engineAggregator{events: &runner.events}
	reporter := &engineReporter{store: store, events: &runner.events, reused: true}
	engine := workflow.NewExecutionEngine(store, runner, aggregator, reporter)

	result, err := engine.Execute(context.Background(), workflow.EngineRequest{
		RunID: "run-001", Owner: "worker-a", OutputPath: "restored.md",
		Lease: workflow.LeaseSettings{TTL: time.Minute, RenewInterval: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.events, []string{"reporter"}) || !result.Report.Reused {
		t.Fatalf("execution events = %v, result = %#v", runner.events, result)
	}
}

type engineStore struct {
	run domain.Run
}

func (store *engineStore) GetRun(context.Context, string) (domain.Run, error) {
	return store.run, nil
}

type engineRunner struct {
	store  *engineStore
	events []string
}

func (runner *engineRunner) Run(context.Context, workflow.RunRequest) (workflow.RunResult, error) {
	runner.events = append(runner.events, "runner")
	runner.store.run.Status = domain.RunStatusAggregating
	return workflow.RunResult{RunID: runner.store.run.ID, Completed: 1}, nil
}

type engineAggregator struct {
	events *[]string
}

func (aggregator *engineAggregator) Aggregate(context.Context, string, time.Time) (verifier.AggregatorResult, error) {
	*aggregator.events = append(*aggregator.events, "aggregator")
	return verifier.AggregatorResult{Candidates: 1, Findings: 1, Advisory: 1}, nil
}

type engineReporter struct {
	store  *engineStore
	events *[]string
	reused bool
}

func (reporter *engineReporter) Generate(_ context.Context, request report.GenerateRequest) (report.GenerateResult, error) {
	*reporter.events = append(*reporter.events, "reporter")
	reporter.store.run.Status = domain.RunStatusReported
	return report.GenerateResult{Report: domain.Report{RunID: request.RunID, OutputPath: request.OutputPath}, Reused: reporter.reused}, nil
}
