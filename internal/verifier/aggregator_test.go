package verifier_test

import (
	"context"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/verifier"
)

func TestAggregatorVerifiesDeduplicatesAndCheckpointsFindings(t *testing.T) {
	now := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	secretA := testCandidate("candidate-secret-a", "security", 2, "配置中包含硬编码 API Key")
	secretB := testCandidate("candidate-secret-b", "security", 2, "配置中包含硬编码 API Key")
	advisory := testCandidate("candidate-advisory", "performance", 3, "循环内可能产生额外分配")
	store := &aggregatorStore{
		candidates: []domain.CandidateFindingRecord{secretB, advisory, secretA},
		unit: domain.ReviewUnit{
			ID: "unit-001", RunID: "run-001", FilePath: "config.yaml",
			DiffHunk: "@@ -0,0 +1,3 @@\n+package config\n+api_key: \"<REDACTED:API_KEY:1>\"\n+items := append([]string{}, source...)\n",
		},
	}
	aggregator := verifier.NewAggregator(store, verifier.NewDefault())

	result, err := aggregator.Aggregate(context.Background(), "run-001", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidates != 3 || result.Findings != 2 || result.Confirmed != 1 || result.Advisory != 1 || result.Duplicates != 1 {
		t.Fatalf("aggregate result = %#v", result)
	}
	if store.unitReads != 1 {
		t.Fatalf("unit reads = %d, want cached single read", store.unitReads)
	}
	if len(store.saved) != 2 {
		t.Fatalf("saved findings = %#v", store.saved)
	}
	if store.saved[0].ID == "" || store.saved[1].ID == "" {
		t.Fatalf("saved findings have empty IDs: %#v", store.saved)
	}
}

func TestAggregatorUsesAgentToolEvidenceWhenVerifyingFinding(t *testing.T) {
	now := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	candidate := testCandidate("candidate-secret", "security", 2, "配置中包含硬编码 API Key")
	store := &aggregatorStore{
		candidates: []domain.CandidateFindingRecord{candidate},
		unit: domain.ReviewUnit{
			ID: "unit-001", RunID: "run-001", FilePath: "config.yaml",
			DiffHunk: "@@ -0,0 +1,2 @@\n+package config\n+api_key: \"<REDACTED:API_KEY:1>\"\n",
		},
		steps: []domain.AgentStep{{
			RunID: "run-001", UnitID: "unit-001", Round: 1,
			ToolCalls: []domain.AgentToolCall{{
				ID: "tool-1", Name: "read_file", Arguments: `{"path":"config.yaml"}`,
			}},
			ToolResults: []domain.AgentToolResult{{
				CallID: "tool-1", Name: "read_file",
				Content: `{"path":"config.yaml","sha":"head-sha","content":"package config\napi_key: \"<REDACTED:API_KEY:1>\"\n","redactions":1}`,
			}},
		}},
	}

	result, err := verifier.NewAggregator(store, verifier.NewDefault()).Aggregate(context.Background(), "run-001", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Confirmed != 1 || len(store.saved) != 1 {
		t.Fatalf("aggregate result = %#v, findings = %#v", result, store.saved)
	}
	if store.saved[0].VerificationSource != "tool:read_file+rule:redacted_secret_assignment" {
		t.Fatalf("verification source = %q", store.saved[0].VerificationSource)
	}
	if store.stepReads != 1 {
		t.Fatalf("agent step reads = %d, want 1", store.stepReads)
	}
}

func TestAggregatorIncludesConfirmedCheckerDiagnosticWithoutLLMCandidate(t *testing.T) {
	now := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	store := &checkerAggregatorStore{
		aggregatorStore: aggregatorStore{},
		diagnostics: []domain.CheckerDiagnostic{{
			ID: "diagnostic-1", RunID: "run-001", TraceID: "trace-checker", Checker: "staticcheck",
			File: "main.go", Line: 4, Column: 2, Code: "SA1006", Message: "invalid printf argument", Severity: "high", CreatedAt: now,
		}},
	}
	result, err := verifier.NewAggregator(store, verifier.NewDefault()).Aggregate(context.Background(), "run-001", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Candidates != 0 || result.Findings != 1 || result.Confirmed != 1 || len(store.saved) != 1 {
		t.Fatalf("result = %#v, findings = %#v", result, store.saved)
	}
	if store.saved[0].CandidateID != "" || store.saved[0].VerificationSource != "checker:staticcheck" || store.saved[0].TraceID != "trace-checker" {
		t.Fatalf("checker finding = %#v", store.saved[0])
	}
}

type aggregatorStore struct {
	candidates []domain.CandidateFindingRecord
	unit       domain.ReviewUnit
	steps      []domain.AgentStep
	unitReads  int
	stepReads  int
	saved      []domain.VerifiedFinding
}

type checkerAggregatorStore struct {
	aggregatorStore
	diagnostics []domain.CheckerDiagnostic
}

func (store *checkerAggregatorStore) ListCheckerDiagnostics(context.Context, string) ([]domain.CheckerDiagnostic, error) {
	return append([]domain.CheckerDiagnostic(nil), store.diagnostics...), nil
}

func (store *aggregatorStore) ListCandidateFindings(context.Context, string) ([]domain.CandidateFindingRecord, error) {
	return append([]domain.CandidateFindingRecord(nil), store.candidates...), nil
}

func (store *aggregatorStore) GetReviewUnit(context.Context, string) (domain.ReviewUnit, error) {
	store.unitReads++
	return store.unit, nil
}

func (store *aggregatorStore) ListAgentSteps(context.Context, string) ([]domain.AgentStep, error) {
	store.stepReads++
	return append([]domain.AgentStep(nil), store.steps...), nil
}

func (store *aggregatorStore) ReplaceVerifiedFindings(_ context.Context, _ string, findings []domain.VerifiedFinding) error {
	store.saved = append([]domain.VerifiedFinding(nil), findings...)
	return nil
}
