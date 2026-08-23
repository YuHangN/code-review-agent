package llm_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/llm"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

func TestGatewayReservesBeforeCallAndSettlesActualUsage(t *testing.T) {
	manager := newBudgetManager(t, 100)
	provider := &llm.FakeProvider{Response: llm.Response{
		Content: `{"findings":[]}`,
		Usage:   &llm.TokenUsage{InputTokens: 4, OutputTokens: 2},
	}}
	gateway := llm.NewGateway(manager, fixedCounter(5), map[string]llm.Provider{"fake": provider}, testTiers())

	response, err := gateway.Call(context.Background(), callRequest("call-1"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != `{"findings":[]}` {
		t.Fatalf("content = %q", response.Content)
	}
	requests := provider.Requests()
	if len(requests) != 1 || requests[0].Model != "fake-reviewer" || requests[0].MaxOutputTokens != 10 {
		t.Fatalf("provider requests = %#v", requests)
	}
	summary, err := manager.Summary(context.Background(), "run-001")
	if err != nil {
		t.Fatal(err)
	}
	// 4 个输入 token × 2 + 2 个输出 token × 4 = 16 微美元。
	if summary.ActualMicros != 16 || summary.ReservedMicros != 0 || summary.CommittedMicros != 16 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestGatewayDoesNotCallProviderWhenBudgetCannotBeReserved(t *testing.T) {
	manager := newBudgetManager(t, 20)
	provider := &llm.FakeProvider{}
	gateway := llm.NewGateway(manager, fixedCounter(5), map[string]llm.Provider{"fake": provider}, testTiers())

	_, err := gateway.Call(context.Background(), callRequest("call-1"))
	if !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("Call error = %v, want ErrLimitExceeded", err)
	}
	if len(provider.Requests()) != 0 {
		t.Fatalf("provider was called: %#v", provider.Requests())
	}
}

func TestGatewayFallsBackWhenPreferredTierCannotReserveBudget(t *testing.T) {
	manager := newBudgetManager(t, 60)
	strong := &llm.FakeProvider{}
	economy := &llm.FakeProvider{Response: llm.Response{
		Content: `{"findings":[]}`,
		Usage:   &llm.TokenUsage{InputTokens: 5, OutputTokens: 1},
	}}
	gateway := llm.NewGateway(manager, fixedCounter(5), map[string]llm.Provider{
		"strong-provider":  strong,
		"economy-provider": economy,
	}, fallbackTiers())
	request := callRequest("call-1")
	request.TierOrder = []string{"strong", "economy"}

	response, err := gateway.Call(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Tier != "economy" {
		t.Fatalf("selected tier = %q, want economy", response.Tier)
	}
	if len(strong.Requests()) != 0 || len(economy.Requests()) != 1 {
		t.Fatalf("provider calls: strong=%d economy=%d", len(strong.Requests()), len(economy.Requests()))
	}
}

func TestGatewayReturnsLimitWhenNoFallbackTierFitsBudget(t *testing.T) {
	manager := newBudgetManager(t, 40)
	strong := &llm.FakeProvider{}
	economy := &llm.FakeProvider{}
	gateway := llm.NewGateway(manager, fixedCounter(5), map[string]llm.Provider{
		"strong-provider":  strong,
		"economy-provider": economy,
	}, fallbackTiers())
	request := callRequest("call-1")
	request.TierOrder = []string{"strong", "economy"}

	_, err := gateway.Call(context.Background(), request)
	if !errors.Is(err, budget.ErrLimitExceeded) {
		t.Fatalf("Call error = %v, want ErrLimitExceeded", err)
	}
	if len(strong.Requests()) != 0 || len(economy.Requests()) != 0 {
		t.Fatalf("provider was called: strong=%d economy=%d", len(strong.Requests()), len(economy.Requests()))
	}
}

func TestGatewayDoesNotFallbackOnProviderFailure(t *testing.T) {
	manager := newBudgetManager(t, 200)
	providerErr := errors.New("provider unavailable")
	strong := &llm.FakeProvider{Err: providerErr}
	economy := &llm.FakeProvider{}
	gateway := llm.NewGateway(manager, fixedCounter(5), map[string]llm.Provider{
		"strong-provider":  strong,
		"economy-provider": economy,
	}, fallbackTiers())
	request := callRequest("call-1")
	request.TierOrder = []string{"strong", "economy"}

	_, err := gateway.Call(context.Background(), request)
	if !errors.Is(err, providerErr) {
		t.Fatalf("Call error = %v, want provider error", err)
	}
	if len(strong.Requests()) != 1 || len(economy.Requests()) != 0 {
		t.Fatalf("provider calls: strong=%d economy=%d", len(strong.Requests()), len(economy.Requests()))
	}
}

func TestGatewayReleasesReservationWhenProviderFails(t *testing.T) {
	manager := newBudgetManager(t, 100)
	provider := &llm.FakeProvider{Err: errors.New("temporary provider failure")}
	gateway := llm.NewGateway(manager, fixedCounter(5), map[string]llm.Provider{"fake": provider}, testTiers())

	if _, err := gateway.Call(context.Background(), callRequest("call-1")); err == nil {
		t.Fatal("Call succeeded, want provider error")
	}
	summary, err := manager.Summary(context.Background(), "run-001")
	if err != nil {
		t.Fatal(err)
	}
	if summary.CommittedMicros != 0 {
		t.Fatalf("committed micros = %d, want released reservation", summary.CommittedMicros)
	}
}

func TestGatewayChargesFullReservationWhenUsageIsMissing(t *testing.T) {
	manager := newBudgetManager(t, 100)
	provider := &llm.FakeProvider{Response: llm.Response{Content: `{"findings":[]}`}}
	gateway := llm.NewGateway(manager, fixedCounter(5), map[string]llm.Provider{"fake": provider}, testTiers())

	if _, err := gateway.Call(context.Background(), callRequest("call-1")); err != nil {
		t.Fatal(err)
	}
	summary, err := manager.Summary(context.Background(), "run-001")
	if err != nil {
		t.Fatal(err)
	}
	// 5 个输入 token × 2 + 最多 10 个输出 token × 4 = 50 微美元。
	if summary.ActualMicros != 50 {
		t.Fatalf("actual micros = %d, want conservative charge 50", summary.ActualMicros)
	}
}

func testTiers() map[string]llm.Tier {
	return map[string]llm.Tier{
		"economy": {
			Provider:                    "fake",
			Model:                       "fake-reviewer",
			InputPriceMicrosPerMillion:  2_000_000,
			OutputPriceMicrosPerMillion: 4_000_000,
			MaxOutputTokens:             10,
		},
	}
}

func fallbackTiers() map[string]llm.Tier {
	return map[string]llm.Tier{
		"strong": {
			Provider:                    "strong-provider",
			Model:                       "strong-reviewer",
			InputPriceMicrosPerMillion:  4_000_000,
			OutputPriceMicrosPerMillion: 8_000_000,
			MaxOutputTokens:             10,
		},
		"economy": {
			Provider:                    "economy-provider",
			Model:                       "economy-reviewer",
			InputPriceMicrosPerMillion:  2_000_000,
			OutputPriceMicrosPerMillion: 4_000_000,
			MaxOutputTokens:             10,
		},
	}
}

func callRequest(id string) llm.CallRequest {
	return llm.CallRequest{ID: id, RunID: "run-001", UnitID: "unit-001", TierOrder: []string{"economy"}, Prompt: "review this diff"}
}

type fixedCounter int64

func (counter fixedCounter) CountInputTokens(context.Context, string, string) (int64, error) {
	return int64(counter), nil
}

func newBudgetManager(t *testing.T, limit int64) budget.Manager {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	unit := domain.ReviewUnit{ID: "unit-001", RunID: "run-001", UnitKey: "unit-key", FilePath: "main.go", Risk: "high", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now}
	run := domain.Run{ID: "run-001", SourceURL: "https://example.test/pr/1", Provider: "fake", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: limit, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, []domain.ReviewUnit{unit}); err != nil {
		t.Fatal(err)
	}
	return budget.NewManager(store)
}
