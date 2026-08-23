package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/report"
	"github.com/YuHangN/code-review-agent/internal/verifier"
)

var (
	ErrInvalidEngineRequest = errors.New("invalid execution engine request")
	ErrRunNotExecutable     = errors.New("run status is not executable")
)

// EngineStore 提供状态驱动执行所需的 Run checkpoint。
type EngineStore interface {
	GetRun(ctx context.Context, runID string) (domain.Run, error)
}

type EngineRunner interface {
	Run(ctx context.Context, request RunRequest) (RunResult, error)
}

type EngineAggregator interface {
	Aggregate(ctx context.Context, runID string, now time.Time) (verifier.AggregatorResult, error)
}

type EngineReporter interface {
	Generate(ctx context.Context, request report.GenerateRequest) (report.GenerateResult, error)
}

// ExecutionEngine 让新执行和恢复执行共享同一条状态机路径。
type ExecutionEngine struct {
	store      EngineStore
	runner     EngineRunner
	aggregator EngineAggregator
	reporter   EngineReporter
}

type EngineRequest struct {
	RunID      string
	Owner      string
	OutputPath string
	Lease      LeaseSettings
}

type EngineResult struct {
	Status      domain.RunStatus
	Review      RunResult
	Aggregation verifier.AggregatorResult
	Report      report.GenerateResult
}

func NewExecutionEngine(store EngineStore, runner EngineRunner, aggregator EngineAggregator, reporter EngineReporter) ExecutionEngine {
	return ExecutionEngine{store: store, runner: runner, aggregator: aggregator, reporter: reporter}
}

// Execute 从当前持久化状态继续，绝不重做已经完成的阶段。
func (engine ExecutionEngine) Execute(ctx context.Context, request EngineRequest) (EngineResult, error) {
	if engine.store == nil || engine.runner == nil || engine.aggregator == nil || engine.reporter == nil || request.RunID == "" || request.Owner == "" || request.OutputPath == "" {
		return EngineResult{}, ErrInvalidEngineRequest
	}
	if err := request.Lease.Validate(); err != nil {
		return EngineResult{}, err
	}
	result := EngineResult{}
	for transitions := 0; transitions < 3; transitions++ {
		run, err := engine.store.GetRun(ctx, request.RunID)
		if err != nil {
			return result, fmt.Errorf("get execution run: %w", err)
		}
		switch run.Status {
		case domain.RunStatusPlanned, domain.RunStatusReviewing:
			result.Review, err = engine.runner.Run(ctx, RunRequest{RunID: request.RunID, Owner: request.Owner, Lease: request.Lease})
			if err != nil {
				return result, fmt.Errorf("run review units: %w", err)
			}
		case domain.RunStatusAggregating:
			result.Aggregation, err = engine.aggregator.Aggregate(ctx, request.RunID, time.Now().UTC())
			if err != nil {
				return result, fmt.Errorf("aggregate findings: %w", err)
			}
			result.Report, err = engine.reporter.Generate(ctx, report.GenerateRequest{RunID: request.RunID, OutputPath: request.OutputPath, Now: time.Now().UTC()})
			if err != nil {
				return result, fmt.Errorf("generate report: %w", err)
			}
			result.Status = domain.RunStatusReported
			return result, nil
		case domain.RunStatusReported:
			result.Report, err = engine.reporter.Generate(ctx, report.GenerateRequest{RunID: request.RunID, OutputPath: request.OutputPath, Now: time.Now().UTC()})
			if err != nil {
				return result, fmt.Errorf("restore report: %w", err)
			}
			result.Status = domain.RunStatusReported
			return result, nil
		default:
			return result, fmt.Errorf("%w: %s", ErrRunNotExecutable, run.Status)
		}
	}
	return result, fmt.Errorf("%w: transition limit exceeded", ErrRunNotExecutable)
}
