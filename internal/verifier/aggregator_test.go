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

type aggregatorStore struct {
	candidates []domain.CandidateFindingRecord
	unit       domain.ReviewUnit
	unitReads  int
	saved      []domain.VerifiedFinding
}

func (store *aggregatorStore) ListCandidateFindings(context.Context, string) ([]domain.CandidateFindingRecord, error) {
	return append([]domain.CandidateFindingRecord(nil), store.candidates...), nil
}

func (store *aggregatorStore) GetReviewUnit(context.Context, string) (domain.ReviewUnit, error) {
	store.unitReads++
	return store.unit, nil
}

func (store *aggregatorStore) ReplaceVerifiedFindings(_ context.Context, _ string, findings []domain.VerifiedFinding) error {
	store.saved = append([]domain.VerifiedFinding(nil), findings...)
	return nil
}
