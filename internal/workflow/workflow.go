// Package workflow 负责从持久化状态继续一次 Review，直到生成报告。
package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/report"
	"github.com/YuHangN/code-review-agent/internal/review"
	"github.com/YuHangN/code-review-agent/internal/verifier"
)

var (
	ErrInvalidWorkflowRequest = errors.New("invalid workflow request")
	ErrWorkflowState          = errors.New("run status is not executable")
)

// StateStore 是 Workflow 调度、lease 和状态推进所需的持久化能力。
type StateStore interface {
	GetRun(ctx context.Context, runID string) (domain.Run, error)
	ClaimRun(ctx context.Context, runID, owner string, now time.Time, ttl time.Duration) (domain.Run, error)
	ListUnits(ctx context.Context, runID string) ([]domain.ReviewUnit, error)
	AdvanceRunToChecking(ctx context.Context, runID, owner string, now time.Time) error
	AdvanceCheckingToAggregating(ctx context.Context, runID, owner string, now time.Time) error
	ReleaseRunLease(ctx context.Context, runID, owner string) error
}

// UnitProcessor 处理一个 Review Unit，并负责保存它的 checkpoint。
type UnitProcessor interface {
	Process(ctx context.Context, unitID string, now time.Time) (review.UnitOutcome, error)
}

// CheckerProcessor 执行仓库级静态检查并保存独立 checkpoint。
type CheckerProcessor interface {
	Process(ctx context.Context, runID, owner string, now time.Time) error
}

// FindingAggregator 验证、去重并保存最终 Finding。
type FindingAggregator interface {
	Aggregate(ctx context.Context, runID string, now time.Time) (verifier.AggregatorResult, error)
}

// ReportGenerator 生成或恢复权威 Markdown 报告。
type ReportGenerator interface {
	Generate(ctx context.Context, request report.GenerateRequest) (report.GenerateResult, error)
}

// Workflow 根据 Run 状态继续审查、聚合和报告阶段。
type Workflow struct {
	store      StateStore
	processor  UnitProcessor
	checker    CheckerProcessor
	aggregator FindingAggregator
	reporter   ReportGenerator
}

// ExecuteRequest 指定一次 Workflow 执行所需的 Run、进程身份和输出位置。
type ExecuteRequest struct {
	RunID      string
	Owner      string
	OutputPath string
	Lease      LeaseSettings
}

// UnitSummary 记录本次调用实际处理的 Unit 数量。
type UnitSummary struct {
	Completed         int
	FailedRecoverable int
	SkippedBudget     int
}

// Result 汇总 Workflow 最终状态和各阶段产物。
type Result struct {
	Status      domain.RunStatus
	Units       UnitSummary
	Aggregation verifier.AggregatorResult
	Report      report.GenerateResult
}

func New(store StateStore, processor UnitProcessor, aggregator FindingAggregator, reporter ReportGenerator) Workflow {
	return Workflow{store: store, processor: processor, checker: noOpChecker{}, aggregator: aggregator, reporter: reporter}
}

type noOpChecker struct{}

func (noOpChecker) Process(context.Context, string, string, time.Time) error { return nil }

func NewWithChecker(store StateStore, processor UnitProcessor, checker CheckerProcessor, aggregator FindingAggregator, reporter ReportGenerator) Workflow {
	return Workflow{store: store, processor: processor, checker: checker, aggregator: aggregator, reporter: reporter}
}

// Execute 从数据库中的当前状态继续，不重做已经完成的阶段。
func (workflow Workflow) Execute(ctx context.Context, request ExecuteRequest) (Result, error) {
	if workflow.store == nil || workflow.processor == nil || workflow.aggregator == nil || workflow.reporter == nil || request.RunID == "" || request.Owner == "" || request.OutputPath == "" {
		return Result{}, ErrInvalidWorkflowRequest
	}
	if err := request.Lease.Validate(); err != nil {
		return Result{}, err
	}
	result := Result{}
	for transitions := 0; transitions < 5; transitions++ {
		run, err := workflow.store.GetRun(ctx, request.RunID)
		if err != nil {
			return result, fmt.Errorf("get workflow run: %w", err)
		}
		switch run.Status {
		case domain.RunStatusPlanned, domain.RunStatusReviewing:
			result.Units, err = workflow.processUnits(ctx, request)
			if err != nil {
				return result, fmt.Errorf("process review units: %w", err)
			}
		case domain.RunStatusChecking:
			if workflow.checker == nil {
				return result, fmt.Errorf("%w: checker stage is not configured", ErrWorkflowState)
			}
			if err = workflow.processCheckers(ctx, request); err != nil {
				return result, fmt.Errorf("process checkers: %w", err)
			}
		case domain.RunStatusAggregating:
			result.Aggregation, err = workflow.aggregator.Aggregate(ctx, request.RunID, time.Now().UTC())
			if err != nil {
				return result, fmt.Errorf("aggregate findings: %w", err)
			}
			result.Report, err = workflow.reporter.Generate(ctx, report.GenerateRequest{RunID: request.RunID, OutputPath: request.OutputPath, Now: time.Now().UTC()})
			if err != nil {
				return result, fmt.Errorf("generate report: %w", err)
			}
			result.Status = domain.RunStatusReported
			return result, nil
		case domain.RunStatusReported:
			result.Report, err = workflow.reporter.Generate(ctx, report.GenerateRequest{RunID: request.RunID, OutputPath: request.OutputPath, Now: time.Now().UTC()})
			if err != nil {
				return result, fmt.Errorf("restore report: %w", err)
			}
			result.Status = domain.RunStatusReported
			return result, nil
		default:
			return result, fmt.Errorf("%w: %s", ErrWorkflowState, run.Status)
		}
	}
	return result, fmt.Errorf("%w: transition limit exceeded", ErrWorkflowState)
}

func (workflow Workflow) processUnits(ctx context.Context, request ExecuteRequest) (summary UnitSummary, resultErr error) {
	if _, err := workflow.store.ClaimRun(ctx, request.RunID, request.Owner, time.Now().UTC(), request.Lease.TTL); err != nil {
		return summary, fmt.Errorf("claim run: %w", err)
	}
	units, err := workflow.store.ListUnits(ctx, request.RunID)
	if err != nil {
		releaseErr := workflow.store.ReleaseRunLease(context.WithoutCancel(ctx), request.RunID, request.Owner)
		return summary, errors.Join(fmt.Errorf("list review units: %w", err), releaseErr)
	}
	units = runnableUnits(units)
	sortUnits(units)

	leaseCtx, stopLease := context.WithCancel(ctx)
	leaseErrors := workflow.maintainLease(leaseCtx, request)
	defer func() {
		stopLease()
		for leaseErr := range leaseErrors {
			if leaseErr != nil {
				resultErr = errors.Join(resultErr, leaseErr)
			}
		}
		if releaseErr := workflow.store.ReleaseRunLease(context.WithoutCancel(ctx), request.RunID, request.Owner); releaseErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release run lease: %w", releaseErr))
		}
	}()

	var unitErrors []error
	for _, unit := range units {
		if err := pollLeaseError(leaseErrors); err != nil {
			return summary, err
		}
		outcome, processErr := workflow.processor.Process(ctx, unit.ID, time.Now().UTC())
		countUnit(&summary, outcome.Status)
		if processErr != nil {
			unitErrors = append(unitErrors, fmt.Errorf("process unit %s: %w", unit.ID, processErr))
		}
		if err := pollLeaseError(leaseErrors); err != nil {
			return summary, err
		}
		if err := ctx.Err(); err != nil {
			return summary, err
		}
	}
	if len(unitErrors) > 0 {
		return summary, errors.Join(unitErrors...)
	}
	// 推进状态前先等待续期循环退出，避免漏掉一个正在返回的 lease 失败。
	stopLease()
	if err := waitForLease(leaseErrors); err != nil {
		return summary, err
	}
	if err := workflow.store.AdvanceRunToChecking(ctx, request.RunID, request.Owner, time.Now().UTC()); err != nil {
		return summary, fmt.Errorf("advance run to checking: %w", err)
	}
	return summary, nil
}

func (workflow Workflow) processCheckers(ctx context.Context, request ExecuteRequest) (resultErr error) {
	if _, err := workflow.store.ClaimRun(ctx, request.RunID, request.Owner, time.Now().UTC(), request.Lease.TTL); err != nil {
		return fmt.Errorf("claim run: %w", err)
	}
	leaseCtx, stopLease := context.WithCancel(ctx)
	leaseErrors := workflow.maintainLease(leaseCtx, request)
	defer func() {
		stopLease()
		for leaseErr := range leaseErrors {
			resultErr = errors.Join(resultErr, leaseErr)
		}
		if releaseErr := workflow.store.ReleaseRunLease(context.WithoutCancel(ctx), request.RunID, request.Owner); releaseErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release run lease: %w", releaseErr))
		}
	}()
	if err := workflow.checker.Process(ctx, request.RunID, request.Owner, time.Now().UTC()); err != nil {
		return err
	}
	stopLease()
	if err := waitForLease(leaseErrors); err != nil {
		return err
	}
	if err := workflow.store.AdvanceCheckingToAggregating(ctx, request.RunID, request.Owner, time.Now().UTC()); err != nil {
		return fmt.Errorf("advance checking to aggregating: %w", err)
	}
	return nil
}

func waitForLease(leaseErrors <-chan error) error {
	var result error
	for err := range leaseErrors {
		result = errors.Join(result, err)
	}
	return result
}

func (workflow Workflow) maintainLease(ctx context.Context, request ExecuteRequest) <-chan error {
	leaseErrors := make(chan error, 1)
	go func() {
		defer close(leaseErrors)
		ticker := time.NewTicker(request.Lease.RenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := workflow.store.ClaimRun(ctx, request.RunID, request.Owner, now.UTC(), request.Lease.TTL); err != nil {
					leaseErrors <- fmt.Errorf("renew run lease: %w", err)
					return
				}
			}
		}
	}()
	return leaseErrors
}

func runnableUnits(units []domain.ReviewUnit) []domain.ReviewUnit {
	result := make([]domain.ReviewUnit, 0, len(units))
	for _, unit := range units {
		switch unit.Status {
		case domain.UnitStatusPending, domain.UnitStatusRunning, domain.UnitStatusFailedRecoverable:
			result = append(result, unit)
		}
	}
	return result
}

func sortUnits(units []domain.ReviewUnit) {
	sort.SliceStable(units, func(i, j int) bool {
		left, right := unitRiskRank(units[i].Risk), unitRiskRank(units[j].Risk)
		if left != right {
			return left < right
		}
		return units[i].UnitKey < units[j].UnitKey
	})
}

func unitRiskRank(risk string) int {
	switch risk {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

func countUnit(summary *UnitSummary, status domain.UnitStatus) {
	switch status {
	case domain.UnitStatusCompleted:
		summary.Completed++
	case domain.UnitStatusFailedRecoverable:
		summary.FailedRecoverable++
	case domain.UnitStatusSkippedBudget:
		summary.SkippedBudget++
	}
}

func pollLeaseError(leaseErrors <-chan error) error {
	select {
	case err, ok := <-leaseErrors:
		if ok && err != nil {
			return err
		}
		return nil
	default:
		return nil
	}
}
