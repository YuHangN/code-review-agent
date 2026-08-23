// Package aggregation 汇总 LLM 与确定性 Checker 的结果，生成最终 Finding checkpoint。
package aggregation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

var ErrInvalidAggregation = errors.New("invalid finding aggregation")

// Store 是聚合候选问题和保存最终结果所需的最小持久化接口。
type Store interface {
	ListCandidateFindings(ctx context.Context, runID string) ([]domain.CandidateFindingRecord, error)
	ReplaceFindings(ctx context.Context, runID string, findings []domain.Finding) error
}

type checkerDiagnosticStore interface {
	ListCheckerDiagnostics(ctx context.Context, runID string) ([]domain.CheckerDiagnostic, error)
}

// Result 汇总候选数量、最终数量及证据等级覆盖情况。
type Result struct {
	Candidates int
	Findings   int
	Confirmed  int
	Advisory   int
	Duplicates int
}

// Aggregator 将 LLM Candidate 固定标为 advisory，将 Checker Diagnostic 固定标为 confirmed。
type Aggregator struct {
	store Store
}

func New(store Store) Aggregator {
	return Aggregator{store: store}
}

// Aggregate 可安全重复执行：相同输入生成相同 fingerprint，并原子替换最终结果。
func (aggregator Aggregator) Aggregate(ctx context.Context, runID string, now time.Time) (Result, error) {
	if aggregator.store == nil || runID == "" || now.IsZero() {
		return Result{}, ErrInvalidAggregation
	}
	candidates, err := aggregator.store.ListCandidateFindings(ctx, runID)
	if err != nil {
		return Result{}, fmt.Errorf("list candidate findings: %w", err)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	result := Result{Candidates: len(candidates)}
	byFingerprint := make(map[string]domain.Finding)

	if checkerStore, ok := aggregator.store.(checkerDiagnosticStore); ok {
		diagnostics, err := checkerStore.ListCheckerDiagnostics(ctx, runID)
		if err != nil {
			return Result{}, fmt.Errorf("list checker diagnostics: %w", err)
		}
		for _, diagnostic := range diagnostics {
			if diagnostic.RunID != runID {
				return Result{}, fmt.Errorf("checker diagnostic %q belongs to another run", diagnostic.ID)
			}
			finding := checkerFinding(runID, diagnostic, now)
			addFinding(byFingerprint, finding, &result)
		}
	}

	for _, candidate := range candidates {
		if candidate.RunID != runID {
			return Result{}, fmt.Errorf("candidate %q belongs to another run", candidate.ID)
		}
		finding := advisoryFinding(candidate, now)
		addFinding(byFingerprint, finding, &result)
	}

	findings := make([]domain.Finding, 0, len(byFingerprint))
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
	if err := aggregator.store.ReplaceFindings(ctx, runID, findings); err != nil {
		return Result{}, fmt.Errorf("checkpoint findings: %w", err)
	}
	return result, nil
}

func advisoryFinding(candidate domain.CandidateFindingRecord, now time.Time) domain.Finding {
	fingerprint := fingerprintParts(candidate.Category, candidate.File, candidate.Line, candidate.Title)
	return domain.Finding{
		ID: findingID(candidate.RunID, fingerprint), RunID: candidate.RunID,
		CandidateID: candidate.ID, TraceID: candidate.TraceID, Fingerprint: fingerprint,
		Confidence: domain.ConfidenceAdvisory, VerificationSource: "llm_reasoning_only",
		VerificationReason: "仅有模型推理证据，保留为参考建议",
		Category:           candidate.Category, Severity: candidate.Severity, File: candidate.File,
		Line: candidate.Line, Title: candidate.Title, Explanation: candidate.Explanation,
		Evidence: append([]string(nil), candidate.Evidence...), Suggestion: candidate.Suggestion, CreatedAt: now,
	}
}

func checkerFinding(runID string, diagnostic domain.CheckerDiagnostic, now time.Time) domain.Finding {
	fingerprint := fingerprintParts("correctness", diagnostic.File, diagnostic.Line, diagnostic.Checker+":"+diagnostic.Code)
	return domain.Finding{
		ID: findingID(runID, fingerprint), RunID: runID, TraceID: diagnostic.TraceID, Fingerprint: fingerprint,
		Confidence: domain.ConfidenceConfirmed, VerificationSource: "checker:" + diagnostic.Checker,
		VerificationReason: "受限容器中的确定性静态检查器命中 PR 新增行",
		Category:           "correctness", Severity: diagnostic.Severity, File: diagnostic.File, Line: diagnostic.Line,
		Title: diagnostic.Code + " · " + diagnostic.Message, Explanation: diagnostic.Message,
		Evidence:   []string{fmt.Sprintf("%s 在 %s:%d:%d 输出 %s", diagnostic.Checker, diagnostic.File, diagnostic.Line, diagnostic.Column, diagnostic.Code)},
		Suggestion: "根据静态检查器诊断修复代码，并在本地重新运行对应检查。", CreatedAt: now,
	}
}

func addFinding(findings map[string]domain.Finding, finding domain.Finding, result *Result) {
	current, exists := findings[finding.Fingerprint]
	if !exists {
		findings[finding.Fingerprint] = finding
		return
	}
	result.Duplicates++
	if prefer(finding, current) {
		findings[finding.Fingerprint] = finding
	}
}

func prefer(candidate, current domain.Finding) bool {
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

func fingerprintParts(category, file string, line int, issueType string) string {
	identity := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(file)),
		strconv.Itoa(line),
		strings.ToLower(strings.TrimSpace(category)),
		strings.ToLower(strings.Join(strings.Fields(issueType), " ")),
	}, "\x00")
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", hash[:])
}

func findingID(runID, fingerprint string) string {
	hash := sha256.Sum256([]byte(runID + "\x00" + fingerprint))
	return fmt.Sprintf("finding-%x", hash[:8])
}
