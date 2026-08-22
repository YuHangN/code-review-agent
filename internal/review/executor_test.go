package review_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/review"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

func TestExecutorCompletesUnitAndPersistsCandidatesWithTrace(t *testing.T) {
	ctx := context.Background()
	store := executorStore(t)
	now := time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)
	reviewer := &stubReviewer{result: review.Result{
		Prompt:      "sanitized prompt",
		RawResponse: `{"findings":[{"title":"并发写 map"}]}`,
		Findings: []review.CandidateFinding{{
			Category: "concurrency", Severity: "high", File: "cache.go", Line: 12,
			Title: "并发写 map", Explanation: "共享 map 没有同步", Evidence: []string{"line 12"}, Suggestion: "增加 mutex",
		}},
		Rejections: []review.Rejection{{Index: 1, Reason: "line 不是新增行"}},
	}}
	executor := review.NewExecutor(store, reviewer, "llm_review", "worker-a")

	outcome, err := executor.Execute(ctx, "unit-001", now)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != domain.UnitStatusCompleted || outcome.FindingCount != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
	units, err := store.ListUnits(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Status != domain.UnitStatusCompleted || units[0].Attempt != 1 {
		t.Fatalf("units = %#v", units)
	}
	run, err := store.GetRun(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunStatusReviewing {
		t.Fatalf("run status = %q, want reviewing", run.Status)
	}
	if reviewer.request.Unit.DiffHunk != "@@ -10 +12 @@\n+cache[key] = value\n" || reviewer.request.CallID == "" {
		t.Fatalf("review request = %#v", reviewer.request)
	}
	findings, err := store.ListCandidateFindings(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Detector != "llm_review" || findings[0].Title != "并发写 map" {
		t.Fatalf("findings = %#v", findings)
	}
	trace, err := store.GetReviewTrace(ctx, outcome.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != string(domain.UnitStatusCompleted) || trace.Prompt != "sanitized prompt" || trace.Response == "" || len(trace.Rejections) != 1 {
		t.Fatalf("trace = %#v", trace)
	}
}

func TestExecutorCheckpointsRecoverableReviewerFailure(t *testing.T) {
	ctx := context.Background()
	store := executorStore(t)
	const secret = "sk-provider-1234567890abcdef"
	reviewer := &stubReviewer{result: review.Result{Prompt: "sanitized prompt"}, err: errors.New("provider unavailable: " + secret)}
	executor := review.NewExecutor(store, reviewer, "llm_review", "worker-a")

	outcome, err := executor.Execute(ctx, "unit-001", time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("Execute error = %v", err)
	}
	if outcome.Status != domain.UnitStatusFailedRecoverable || outcome.TraceID == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	units, listErr := store.ListUnits(ctx, "run-001")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if units[0].Status != domain.UnitStatusFailedRecoverable || units[0].Attempt != 1 {
		t.Fatalf("unit = %#v", units[0])
	}
	trace, traceErr := store.GetReviewTrace(ctx, outcome.TraceID)
	if traceErr != nil {
		t.Fatal(traceErr)
	}
	if trace.Status != string(domain.UnitStatusFailedRecoverable) || trace.Prompt != "sanitized prompt" || !strings.Contains(trace.ErrorMessage, "provider unavailable") {
		t.Fatalf("trace = %#v", trace)
	}
	if strings.Contains(trace.ErrorMessage, secret) || !strings.Contains(trace.ErrorMessage, "<REDACTED:TOKEN:1>") {
		t.Fatalf("trace leaked provider secret: %#v", trace)
	}
}

func TestExecutorSkipsUnitWhenBudgetIsExhausted(t *testing.T) {
	ctx := context.Background()
	store := executorStore(t)
	reviewer := &stubReviewer{result: review.Result{Prompt: "sanitized prompt"}, err: budget.ErrLimitExceeded}
	executor := review.NewExecutor(store, reviewer, "llm_review", "worker-a")

	outcome, err := executor.Execute(ctx, "unit-001", time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Execute error = %v, want budget skip without workflow failure", err)
	}
	if outcome.Status != domain.UnitStatusSkippedBudget || outcome.TraceID == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	units, listErr := store.ListUnits(ctx, "run-001")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if units[0].Status != domain.UnitStatusSkippedBudget {
		t.Fatalf("unit status = %q", units[0].Status)
	}
}

func TestExecutorSanitizesEveryPersistedTraceAndFindingField(t *testing.T) {
	ctx := context.Background()
	store := executorStore(t)
	const secret = "sk-persisted-1234567890abcdef"
	reviewer := &stubReviewer{result: review.Result{
		Prompt: "prompt " + secret, RawResponse: "response " + secret,
		Findings: []review.CandidateFinding{{
			Category: "security", Severity: "high", File: "cache.go", Line: 12,
			Title: "title " + secret, Explanation: "explanation " + secret,
			Evidence: []string{"evidence " + secret}, Suggestion: "suggestion " + secret,
		}},
	}}
	executor := review.NewExecutor(store, reviewer, "llm_review", "worker-a")

	outcome, err := executor.Execute(ctx, "unit-001", time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := store.GetReviewTrace(ctx, outcome.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := store.ListCandidateFindings(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	persisted := trace.Prompt + trace.Response + findings[0].Title + findings[0].Explanation + strings.Join(findings[0].Evidence, "") + findings[0].Suggestion
	if strings.Contains(persisted, secret) {
		t.Fatalf("persisted review data leaked secret: %s", persisted)
	}
}

func TestExecutorRejectsProcessThatDoesNotOwnRunLease(t *testing.T) {
	ctx := context.Background()
	store := executorStore(t)
	reviewer := &stubReviewer{}
	executor := review.NewExecutor(store, reviewer, "llm_review", "worker-b")

	if _, err := executor.Execute(ctx, "unit-001", time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)); !errors.Is(err, sqlite.ErrLeaseHeld) {
		t.Fatalf("Execute error = %v, want ErrLeaseHeld", err)
	}
	units, err := store.ListUnits(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if units[0].Status != domain.UnitStatusPending || units[0].Attempt != 0 {
		t.Fatalf("unit changed without lease ownership: %#v", units[0])
	}
}

func TestExecutorCannotCommitAfterLeaseWasTakenOverDuringReview(t *testing.T) {
	ctx := context.Background()
	store := executorStore(t)
	reviewer := &stubReviewer{result: review.Result{Prompt: "prompt", RawResponse: `{"findings":[]}`}}
	reviewer.onReview = func() {
		if _, err := store.ClaimRun(ctx, "run-001", "worker-b", time.Date(2026, 8, 22, 4, 0, 1, 0, time.UTC), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	executor := review.NewExecutor(store, reviewer, "llm_review", "worker-a")

	if _, err := executor.Execute(ctx, "unit-001", time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)); !errors.Is(err, sqlite.ErrLeaseHeld) {
		t.Fatalf("Execute error = %v, want ErrLeaseHeld", err)
	}
	units, err := store.ListUnits(ctx, "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if units[0].Status != domain.UnitStatusRunning {
		t.Fatalf("old owner committed unit status %q", units[0].Status)
	}
}

type stubReviewer struct {
	result   review.Result
	err      error
	request  review.Request
	onReview func()
}

func (stub *stubReviewer) Review(_ context.Context, request review.Request) (review.Result, error) {
	stub.request = request
	if stub.onReview != nil {
		stub.onReview()
	}
	return stub.result, stub.err
}

func executorStore(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	run := domain.Run{ID: "run-001", SourceURL: "https://example.test/pr/1", Provider: "fake", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	unit := domain.ReviewUnit{ID: "unit-001", RunID: run.ID, UnitKey: "key-001", FilePath: "cache.go", StartLine: 12, EndLine: 12, DiffHunk: "@@ -10 +12 @@\n+cache[key] = value\n", Risk: "high", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, []domain.ReviewUnit{unit}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, run.ID, "worker-a", now, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	return store
}
