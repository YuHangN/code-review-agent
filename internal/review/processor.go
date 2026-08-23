package review

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/security"
)

// UnitReviewer 允许 UnitProcessor 使用真实 Reviewer 或离线测试实现。
type UnitReviewer interface {
	Review(ctx context.Context, request Request) (Result, error)
}

// UnitStore 是一次 Unit checkpoint 所需的最小持久化能力。
type UnitStore interface {
	StartReviewUnit(ctx context.Context, unitID, owner string, now time.Time) (domain.ReviewUnit, error)
	CompleteReviewUnit(ctx context.Context, trace domain.ReviewTrace, findings []domain.CandidateFindingRecord, owner string, now time.Time) error
	FinishReviewUnit(ctx context.Context, trace domain.ReviewTrace, status domain.UnitStatus, owner string, now time.Time) error
}

// UnitOutcome 描述一次 Unit 处理写入的最终 checkpoint。
type UnitOutcome struct {
	UnitID       string
	Status       domain.UnitStatus
	TraceID      string
	FindingCount int
}

// UnitProcessor 将持久化 Unit、Reviewer 和 finding trace 串成可恢复处理单元。
type UnitProcessor struct {
	store    UnitStore
	reviewer UnitReviewer
	detector string
	owner    string
}

func NewUnitProcessor(store UnitStore, reviewer UnitReviewer, detector, owner string) UnitProcessor {
	return UnitProcessor{store: store, reviewer: reviewer, detector: detector, owner: owner}
}

// Process 领取一个 Unit，调用 Reviewer，并原子保存成功、可恢复失败或预算跳过 checkpoint。
func (processor UnitProcessor) Process(ctx context.Context, unitID string, now time.Time) (UnitOutcome, error) {
	if processor.store == nil || processor.reviewer == nil || processor.detector == "" || processor.owner == "" || unitID == "" || now.IsZero() {
		return UnitOutcome{}, fmt.Errorf("invalid review unit processor request")
	}
	unit, err := processor.store.StartReviewUnit(ctx, unitID, processor.owner, now)
	if err != nil {
		return UnitOutcome{}, fmt.Errorf("start review unit: %w", err)
	}
	callID := fmt.Sprintf("call-%s-%d", unit.ID, unit.Attempt)
	result, err := processor.reviewer.Review(ctx, Request{CallID: callID, Owner: processor.owner, Unit: unit, Diff: unit.DiffHunk})
	traceID := fmt.Sprintf("trace-%s-%d", unit.ID, unit.Attempt)
	if err != nil {
		status := domain.UnitStatusFailedRecoverable
		if errors.Is(err, budget.ErrLimitExceeded) {
			status = domain.UnitStatusSkippedBudget
		}
		trace := domain.ReviewTrace{
			ID: traceID, RunID: unit.RunID, UnitID: unit.ID, CallID: callID, Detector: processor.detector,
			Status: string(status), Prompt: sanitizePersistedText(result.Prompt), Response: sanitizePersistedText(result.RawResponse),
			ErrorMessage: sanitizePersistedText(err.Error()), CreatedAt: now,
		}
		if checkpointErr := processor.store.FinishReviewUnit(ctx, trace, status, processor.owner, now); checkpointErr != nil {
			return UnitOutcome{}, errors.Join(fmt.Errorf("review unit: %w", err), fmt.Errorf("checkpoint failed unit: %w", checkpointErr))
		}
		outcome := UnitOutcome{UnitID: unit.ID, Status: status, TraceID: traceID}
		if status == domain.UnitStatusSkippedBudget {
			return outcome, nil
		}
		return outcome, fmt.Errorf("review unit: %w", err)
	}
	trace := domain.ReviewTrace{
		ID: traceID, RunID: unit.RunID, UnitID: unit.ID, CallID: callID, Detector: processor.detector,
		Status: string(domain.UnitStatusCompleted), Prompt: sanitizePersistedText(result.Prompt), Response: sanitizePersistedText(result.RawResponse),
		CreatedAt: now,
	}
	for _, rejection := range result.Rejections {
		trace.Rejections = append(trace.Rejections, domain.TraceRejection{Index: rejection.Index, Reason: sanitizePersistedText(rejection.Reason)})
	}
	findings := make([]domain.CandidateFindingRecord, 0, len(result.Findings))
	for index, finding := range result.Findings {
		idInput := fmt.Sprintf("%s\x00%d", traceID, index)
		findingHash := sha256.Sum256([]byte(idInput))
		findings = append(findings, domain.CandidateFindingRecord{
			ID: fmt.Sprintf("candidate-%x", findingHash[:8]), RunID: unit.RunID, UnitID: unit.ID,
			TraceID: traceID, Detector: processor.detector, Category: sanitizePersistedText(finding.Category), Severity: sanitizePersistedText(finding.Severity),
			File: sanitizePersistedText(finding.File), Line: finding.Line, Title: sanitizePersistedText(finding.Title), Explanation: sanitizePersistedText(finding.Explanation),
			Evidence: sanitizeEvidence(finding.Evidence), Suggestion: sanitizePersistedText(finding.Suggestion), CreatedAt: now,
		})
	}
	if err := processor.store.CompleteReviewUnit(ctx, trace, findings, processor.owner, now); err != nil {
		return UnitOutcome{}, fmt.Errorf("complete review unit: %w", err)
	}
	return UnitOutcome{UnitID: unit.ID, Status: domain.UnitStatusCompleted, TraceID: traceID, FindingCount: len(findings)}, nil
}

func sanitizeEvidence(evidence []string) []string {
	result := make([]string, len(evidence))
	for index, item := range evidence {
		result[index] = sanitizePersistedText(item)
	}
	return result
}

func sanitizePersistedText(value string) string {
	result := security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: value})
	return result.Snapshot.Diff
}
