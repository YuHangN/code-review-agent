package report_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/report"
)

func TestGeneratorWritesFileAndCheckpointsReport(t *testing.T) {
	now := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	store := reportStore{
		run:      domain.Run{ID: "run-001", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusAggregating, BudgetLimitMicros: 1_000_000},
		snapshot: domain.ChangeSnapshot{BaseSHA: "base", HeadSHA: "head"},
		units:    []domain.ReviewUnit{{ID: "unit-1", Status: domain.UnitStatusCompleted}},
		findings: []domain.VerifiedFinding{{ID: "finding-1", TraceID: "trace-1", Confidence: domain.ConfidenceAdvisory, VerificationSource: "llm_reasoning_only", VerificationReason: "仅模型推理", Severity: "medium", File: "main.go", Line: 3, Title: "候选问题", Explanation: "问题说明", Evidence: []string{"候选证据"}, Suggestion: "检查实现"}},
		budget:   budget.Summary{ActualMicros: 100, CommittedMicros: 100},
	}
	outputPath := filepath.Join(t.TempDir(), "nested", "report.md")

	result, err := report.NewGenerator(&store).Generate(context.Background(), report.GenerateRequest{RunID: "run-001", OutputPath: outputPath, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != result.Report.Content || store.saved.Content != result.Report.Content {
		t.Fatal("file, result and checkpoint content differ")
	}
	if result.Reused || store.saved.ContentSHA256 == "" || store.saved.OutputPath != outputPath {
		t.Fatalf("generate result = %#v, saved = %#v", result, store.saved)
	}
}

func TestGeneratorReusesReportedCheckpointWithoutRebuilding(t *testing.T) {
	now := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	content := "# Existing Report\n"
	hash := sha256.Sum256([]byte(content))
	stored := domain.Report{RunID: "run-001", OutputPath: "old.md", Content: content, ContentSHA256: fmt.Sprintf("%x", hash[:]), CreatedAt: now.Add(-time.Hour)}
	store := reportStore{run: domain.Run{ID: "run-001", Status: domain.RunStatusReported}, report: stored}
	outputPath := filepath.Join(t.TempDir(), "restored.md")

	result, err := report.NewGenerator(&store).Generate(context.Background(), report.GenerateRequest{RunID: "run-001", OutputPath: outputPath, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	contentOnDisk, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reused || string(contentOnDisk) != content || store.sourceReads != 0 {
		t.Fatalf("reused result = %#v, source reads = %d, content = %q", result, store.sourceReads, contentOnDisk)
	}
	if store.saved.CreatedAt != stored.CreatedAt || store.saved.OutputPath != outputPath {
		t.Fatalf("restored checkpoint = %#v", store.saved)
	}
}

type reportStore struct {
	run         domain.Run
	snapshot    domain.ChangeSnapshot
	units       []domain.ReviewUnit
	findings    []domain.VerifiedFinding
	budget      budget.Summary
	report      domain.Report
	saved       domain.Report
	sourceReads int
}

func (store *reportStore) GetRun(context.Context, string) (domain.Run, error) {
	return store.run, nil
}

func (store *reportStore) GetSnapshot(context.Context, string) (domain.ChangeSnapshot, error) {
	store.sourceReads++
	return store.snapshot, nil
}

func (store *reportStore) ListUnits(context.Context, string) ([]domain.ReviewUnit, error) {
	store.sourceReads++
	return store.units, nil
}

func (store *reportStore) ListVerifiedFindings(context.Context, string) ([]domain.VerifiedFinding, error) {
	store.sourceReads++
	return store.findings, nil
}

func (store *reportStore) BudgetSummary(context.Context, string) (budget.Summary, error) {
	store.sourceReads++
	return store.budget, nil
}

func (store *reportStore) SaveReport(_ context.Context, saved domain.Report, _ time.Time) error {
	store.saved = saved
	return nil
}

func (store *reportStore) GetReport(context.Context, string) (domain.Report, error) {
	return store.report, nil
}
