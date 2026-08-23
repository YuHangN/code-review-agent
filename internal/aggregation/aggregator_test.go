package aggregation_test

import (
	"context"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/aggregation"
	"github.com/YuHangN/code-review-agent/internal/domain"
)

func TestAggregatorKeepsLLMCandidatesAdvisoryAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	secretA := candidate("candidate-secret-a", "security", 2, "配置中包含硬编码 API Key")
	secretB := candidate("candidate-secret-b", "security", 2, "配置中包含硬编码 API Key")
	concurrency := candidate("candidate-concurrency", "concurrency", 6, "共享 map 可能发生并发访问")
	store := &aggregationStore{candidates: []domain.CandidateFindingRecord{secretB, concurrency, secretA}}

	result, err := aggregation.New(store).Aggregate(context.Background(), "run-001", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidates != 3 || result.Findings != 2 || result.Confirmed != 0 || result.Advisory != 2 || result.Duplicates != 1 {
		t.Fatalf("result = %#v", result)
	}
	for _, finding := range store.saved {
		if finding.Confidence != domain.ConfidenceAdvisory || finding.VerificationSource != "llm_reasoning_only" {
			t.Fatalf("LLM finding was promoted: %#v", finding)
		}
	}
}

func TestAggregatorIncludesCheckerDiagnosticAsConfirmed(t *testing.T) {
	now := time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC)
	store := &checkerAggregationStore{
		aggregationStore: aggregationStore{},
		diagnostics: []domain.CheckerDiagnostic{{
			ID: "diagnostic-1", RunID: "run-001", TraceID: "trace-checker", Checker: "staticcheck",
			File: "main.go", Line: 4, Column: 2, Code: "SA1006", Message: "invalid printf argument", Severity: "high", CreatedAt: now,
		}},
	}

	result, err := aggregation.New(store).Aggregate(context.Background(), "run-001", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Findings != 1 || result.Confirmed != 1 || result.Advisory != 0 || len(store.saved) != 1 {
		t.Fatalf("result = %#v, findings = %#v", result, store.saved)
	}
	if store.saved[0].CandidateID != "" || store.saved[0].VerificationSource != "checker:staticcheck" || store.saved[0].TraceID != "trace-checker" {
		t.Fatalf("checker finding = %#v", store.saved[0])
	}
}

func candidate(id, category string, line int, title string) domain.CandidateFindingRecord {
	return domain.CandidateFindingRecord{
		ID: id, RunID: "run-001", UnitID: "unit-001", TraceID: "trace-001",
		Detector: "llm_review", Category: category, Severity: "high",
		File: "config.yaml", Line: line, Title: title,
		Explanation: "模型给出的候选问题", Evidence: []string{"候选证据"}, Suggestion: "修改配置",
	}
}

type aggregationStore struct {
	candidates []domain.CandidateFindingRecord
	saved      []domain.Finding
}

func (store *aggregationStore) ListCandidateFindings(context.Context, string) ([]domain.CandidateFindingRecord, error) {
	return append([]domain.CandidateFindingRecord(nil), store.candidates...), nil
}

func (store *aggregationStore) ReplaceFindings(_ context.Context, _ string, findings []domain.Finding) error {
	store.saved = append([]domain.Finding(nil), findings...)
	return nil
}

type checkerAggregationStore struct {
	aggregationStore
	diagnostics []domain.CheckerDiagnostic
}

func (store *checkerAggregationStore) ListCheckerDiagnostics(context.Context, string) ([]domain.CheckerDiagnostic, error) {
	return append([]domain.CheckerDiagnostic(nil), store.diagnostics...), nil
}
