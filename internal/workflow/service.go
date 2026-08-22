package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

var (
	ErrRunIDRequired   = errors.New("run ID is required")
	ErrUnitRunMismatch = errors.New("review unit run ID does not match run")
)

// Store 是 Workflow 在启动和恢复 Review 时所需的最小持久化能力。
type Store interface {
	CreateRun(ctx context.Context, run domain.Run, units []domain.ReviewUnit) error
	ClaimRun(ctx context.Context, runID, owner string, now time.Time, ttl time.Duration) (domain.Run, error)
	ListUnits(ctx context.Context, runID string) ([]domain.ReviewUnit, error)
}

// Service 编排可恢复的 Run 操作，不依赖具体的 SQLite 实现。
type Service struct {
	store Store
}

// StartRequest 包含一个新 Run 及 Planner 为它生成的 Unit。
type StartRequest struct {
	Run   domain.Run
	Units []domain.ReviewUnit
}

// ResumeResult 只返回仍需要继续审查的 Unit。
type ResumeResult struct {
	Run          domain.Run
	PendingUnits []domain.ReviewUnit
}

// NewService 使用传入的持久化 Store 创建 Workflow Service。
func NewService(store Store) Service {
	return Service{store: store}
}

// Start 校验所有 Unit 都属于目标 Run，然后一起写入 checkpoint。
func (s Service) Start(ctx context.Context, request StartRequest) error {
	if request.Run.ID == "" {
		return ErrRunIDRequired
	}
	for _, unit := range request.Units {
		if unit.RunID != request.Run.ID {
			return ErrUnitRunMismatch
		}
	}
	if err := s.store.CreateRun(ctx, request.Run, request.Units); err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	return nil
}

// Resume 领取 Run 的 lease，并只返回可以再次调度的 Unit。
// 它不执行 Unit；实际执行由后续 Workflow 步骤负责。
func (s Service) Resume(ctx context.Context, runID, owner string, now time.Time, ttl time.Duration) (ResumeResult, error) {
	run, err := s.store.ClaimRun(ctx, runID, owner, now, ttl)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("claim run: %w", err)
	}
	units, err := s.store.ListUnits(ctx, runID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("list review units: %w", err)
	}

	result := ResumeResult{Run: run}
	for _, unit := range units {
		if isResumable(unit.Status) {
			result.PendingUnits = append(result.PendingUnits, unit)
		}
	}
	return result, nil
}

// isResumable 排除 completed 和 skipped_budget Unit，避免恢复时重复处理。
func isResumable(status domain.UnitStatus) bool {
	switch status {
	case domain.UnitStatusPending, domain.UnitStatusRunning, domain.UnitStatusFailedRecoverable:
		return true
	default:
		return false
	}
}
