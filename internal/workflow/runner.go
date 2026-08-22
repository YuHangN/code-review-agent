package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/review"
)

var ErrInvalidRunRequest = errors.New("invalid run request")

// RunCoordinator 提供 Run 调度所需的 lease、恢复和阶段推进能力。
// Service 实现该接口；测试可以替换为不访问数据库的实现。
type RunCoordinator interface {
	Resume(ctx context.Context, runID, owner string, now time.Time, ttl time.Duration) (ResumeResult, error)
	MaintainLease(ctx context.Context, runID, owner string, settings LeaseSettings) (<-chan error, error)
	AdvanceToAggregating(ctx context.Context, runID, owner string, now time.Time) error
	ReleaseLease(ctx context.Context, runID, owner string) error
}

// ReviewUnitExecutor 执行一个 Review Unit，并将结果写入 checkpoint。
type ReviewUnitExecutor interface {
	Execute(ctx context.Context, unitID string, now time.Time) (review.ExecutionOutcome, error)
}

// Runner 负责一次 Run 内部的 Unit 调度，不参与具体审查逻辑。
type Runner struct {
	coordinator RunCoordinator
	executor    ReviewUnitExecutor
}

// RunRequest 指定要执行的 Run、当前进程身份和 lease 配置。
type RunRequest struct {
	RunID string
	Owner string
	Lease LeaseSettings
}

// RunResult 汇总本次恢复实际处理的 Unit 数量。
type RunResult struct {
	RunID             string
	Completed         int
	FailedRecoverable int
	SkippedBudget     int
}

// NewRunner 创建串行 Run 调度器。串行执行让预算预留和恢复顺序保持确定。
func NewRunner(coordinator RunCoordinator, executor ReviewUnitExecutor) Runner {
	return Runner{coordinator: coordinator, executor: executor}
}

// Run 领取 Run、维持 lease，并按风险从高到低处理所有可恢复 Unit。
// 只有没有可恢复失败时，Run 才会进入 aggregating 阶段。
func (runner Runner) Run(ctx context.Context, request RunRequest) (result RunResult, resultErr error) {
	result = RunResult{RunID: request.RunID}
	if runner.coordinator == nil || runner.executor == nil || request.RunID == "" || request.Owner == "" {
		return result, ErrInvalidRunRequest
	}
	if err := request.Lease.Validate(); err != nil {
		return result, err
	}

	resume, err := runner.coordinator.Resume(ctx, request.RunID, request.Owner, time.Now().UTC(), request.Lease.TTL)
	if err != nil {
		return result, fmt.Errorf("resume run: %w", err)
	}
	units := append([]domain.ReviewUnit(nil), resume.PendingUnits...)
	sortReviewUnits(units)

	leaseCtx, stopLease := context.WithCancel(ctx)
	leaseErrors, err := runner.coordinator.MaintainLease(leaseCtx, request.RunID, request.Owner, request.Lease)
	if err != nil {
		stopLease()
		releaseErr := runner.coordinator.ReleaseLease(context.WithoutCancel(ctx), request.RunID, request.Owner)
		return result, errors.Join(fmt.Errorf("maintain lease: %w", err), releaseErr)
	}
	defer func() {
		stopLease()
		// MaintainLease 在退出时关闭 channel；等待它结束可避免续期 goroutine 泄漏。
		for leaseErr := range leaseErrors {
			if leaseErr != nil {
				resultErr = errors.Join(resultErr, leaseErr)
			}
		}
		if releaseErr := runner.coordinator.ReleaseLease(context.WithoutCancel(ctx), request.RunID, request.Owner); releaseErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release run lease: %w", releaseErr))
		}
	}()

	var unitErrors []error
	for _, unit := range units {
		if err := currentLeaseError(leaseErrors); err != nil {
			return result, err
		}
		outcome, executeErr := runner.executor.Execute(ctx, unit.ID, time.Now().UTC())
		countOutcome(&result, outcome.Status)
		if executeErr != nil {
			unitErrors = append(unitErrors, fmt.Errorf("execute unit %s: %w", unit.ID, executeErr))
		}
		if err := currentLeaseError(leaseErrors); err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	if len(unitErrors) > 0 {
		return result, errors.Join(unitErrors...)
	}
	if err := runner.coordinator.AdvanceToAggregating(ctx, request.RunID, request.Owner, time.Now().UTC()); err != nil {
		return result, fmt.Errorf("advance run to aggregating: %w", err)
	}
	return result, nil
}

func sortReviewUnits(units []domain.ReviewUnit) {
	sort.SliceStable(units, func(i, j int) bool {
		left, right := riskRank(units[i].Risk), riskRank(units[j].Risk)
		if left != right {
			return left < right
		}
		return units[i].UnitKey < units[j].UnitKey
	})
}

func riskRank(risk string) int {
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

func countOutcome(result *RunResult, status domain.UnitStatus) {
	switch status {
	case domain.UnitStatusCompleted:
		result.Completed++
	case domain.UnitStatusFailedRecoverable:
		result.FailedRecoverable++
	case domain.UnitStatusSkippedBudget:
		result.SkippedBudget++
	}
}

func currentLeaseError(leaseErrors <-chan error) error {
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
