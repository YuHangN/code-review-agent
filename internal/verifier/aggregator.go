package verifier

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

var ErrInvalidAggregation = errors.New("invalid finding aggregation")

// AggregatorStore 是验证、去重和最终 checkpoint 所需的最小持久化接口。
type AggregatorStore interface {
	ListCandidateFindings(ctx context.Context, runID string) ([]domain.CandidateFindingRecord, error)
	GetReviewUnit(ctx context.Context, unitID string) (domain.ReviewUnit, error)
	ReplaceVerifiedFindings(ctx context.Context, runID string, findings []domain.VerifiedFinding) error
}

// CandidateVerifier 允许 Aggregator 使用默认规则集或测试实现。
type CandidateVerifier interface {
	Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit, now time.Time) domain.VerifiedFinding
}

// AggregatorResult 汇总候选数量、最终数量及证据等级覆盖情况。
type AggregatorResult struct {
	Candidates int
	Findings   int
	Confirmed  int
	Advisory   int
	Duplicates int
}

// Aggregator 将各 Unit 的候选问题统一验证、去重并原子写入最终 checkpoint。
type Aggregator struct {
	store    AggregatorStore
	verifier CandidateVerifier
}

func NewAggregator(store AggregatorStore, verifier CandidateVerifier) Aggregator {
	return Aggregator{store: store, verifier: verifier}
}

// Aggregate 可安全重复执行：相同候选会生成相同 fingerprint，整批结果原子替换。
func (aggregator Aggregator) Aggregate(ctx context.Context, runID string, now time.Time) (AggregatorResult, error) {
	if aggregator.store == nil || aggregator.verifier == nil || runID == "" || now.IsZero() {
		return AggregatorResult{}, ErrInvalidAggregation
	}
	candidates, err := aggregator.store.ListCandidateFindings(ctx, runID)
	if err != nil {
		return AggregatorResult{}, fmt.Errorf("list candidate findings: %w", err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	result := AggregatorResult{Candidates: len(candidates)}
	units := make(map[string]domain.ReviewUnit)
	byFingerprint := make(map[string]domain.VerifiedFinding)
	for _, candidate := range candidates {
		if candidate.RunID != runID {
			return AggregatorResult{}, fmt.Errorf("candidate %q belongs to another run", candidate.ID)
		}
		unit, ok := units[candidate.UnitID]
		if !ok {
			unit, err = aggregator.store.GetReviewUnit(ctx, candidate.UnitID)
			if err != nil {
				return AggregatorResult{}, fmt.Errorf("get candidate unit %s: %w", candidate.UnitID, err)
			}
			if unit.RunID != runID {
				return AggregatorResult{}, fmt.Errorf("unit %q belongs to another run", unit.ID)
			}
			units[candidate.UnitID] = unit
		}
		finding := aggregator.verifier.Verify(candidate, unit, now)
		if current, exists := byFingerprint[finding.Fingerprint]; exists {
			result.Duplicates++
			if preferFinding(finding, current) {
				byFingerprint[finding.Fingerprint] = finding
			}
			continue
		}
		byFingerprint[finding.Fingerprint] = finding
	}

	findings := make([]domain.VerifiedFinding, 0, len(byFingerprint))
	for _, finding := range byFingerprint {
		findings = append(findings, finding)
		if finding.Confidence == domain.ConfidenceConfirmed {
			result.Confirmed++
		} else {
			result.Advisory++
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].ID < findings[j].ID
	})
	result.Findings = len(findings)
	if err := aggregator.store.ReplaceVerifiedFindings(ctx, runID, findings); err != nil {
		return AggregatorResult{}, fmt.Errorf("checkpoint verified findings: %w", err)
	}
	return result, nil
}

func preferFinding(candidate, current domain.VerifiedFinding) bool {
	if candidate.Confidence != current.Confidence {
		return candidate.Confidence == domain.ConfidenceConfirmed
	}
	return severityRank(candidate.Severity) < severityRank(current.Severity)
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}
