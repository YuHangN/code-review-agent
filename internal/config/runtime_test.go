package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRuntimeParsesAndValidatesDurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(`runtime:
  lease_ttl: 60s
  lease_renew_interval: 20s
  sqlite_busy_timeout: 5s
review:
  default_budget_cents: 750
  currency: USD
  max_findings_per_unit: 5
llm:
  request_timeout: 90s
  default_tier: strong
  fallback_order: [strong, economy]
  tiers:
    strong:
      provider: fake
      model: fake-strong-reviewer
      input_price_micros_per_million_tokens: 5000000
      output_price_micros_per_million_tokens: 10000000
      max_output_tokens: 1200
    economy:
      provider: fake
      model: fake-economy-reviewer
      input_price_micros_per_million_tokens: 2000000
      output_price_micros_per_million_tokens: 4000000
      max_output_tokens: 1200
`), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime, err := LoadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.LeaseTTL != time.Minute {
		t.Fatalf("lease TTL = %s, want 1m", runtime.LeaseTTL)
	}
	if runtime.LeaseRenewInterval != 20*time.Second {
		t.Fatalf("lease renew interval = %s, want 20s", runtime.LeaseRenewInterval)
	}
	if runtime.SQLiteBusyTimeout != 5*time.Second {
		t.Fatalf("SQLite busy timeout = %s, want 5s", runtime.SQLiteBusyTimeout)
	}
	if runtime.DefaultBudgetCents != 750 || runtime.Currency != "USD" {
		t.Fatalf("default budget = %d %s, want 750 USD cents", runtime.DefaultBudgetCents, runtime.Currency)
	}
	if runtime.MaxFindingsPerUnit != 5 {
		t.Fatalf("max findings per unit = %d, want 5", runtime.MaxFindingsPerUnit)
	}
	if runtime.DefaultLLMTier != "strong" {
		t.Fatalf("default LLM tier = %q, want strong", runtime.DefaultLLMTier)
	}
	if len(runtime.LLMFallbackOrder) != 2 || runtime.LLMFallbackOrder[0] != "strong" || runtime.LLMFallbackOrder[1] != "economy" {
		t.Fatalf("LLM fallback order = %v, want [strong economy]", runtime.LLMFallbackOrder)
	}
	if runtime.LLMRequestTimeout != 90*time.Second {
		t.Fatalf("LLM request timeout = %s, want 90s", runtime.LLMRequestTimeout)
	}
	strong := runtime.LLMTiers["strong"]
	if strong.Provider != "fake" || strong.Model != "fake-strong-reviewer" || strong.MaxOutputTokens != 1200 {
		t.Fatalf("strong tier = %#v", strong)
	}
	economy := runtime.LLMTiers["economy"]
	if economy.Provider != "fake" || economy.Model != "fake-economy-reviewer" || economy.MaxOutputTokens != 1200 {
		t.Fatalf("economy tier = %#v", economy)
	}
}

func TestLoadRuntimeRejectsUnknownFallbackTier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(`runtime:
  lease_ttl: 60s
  lease_renew_interval: 20s
  sqlite_busy_timeout: 5s
review:
  default_budget_cents: 1000
  currency: USD
  max_findings_per_unit: 5
llm:
  request_timeout: 90s
  default_tier: economy
  fallback_order: [economy, missing]
  tiers:
    economy:
      provider: fake
      model: fake-reviewer
      input_price_micros_per_million_tokens: 2000000
      output_price_micros_per_million_tokens: 4000000
      max_output_tokens: 1200
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRuntime(path); err == nil {
		t.Fatal("LoadRuntime succeeded, want unknown fallback tier error")
	}
}

func TestLoadRuntimeRejectsRenewIntervalAtOrAfterTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(`runtime:
  lease_ttl: 20s
  lease_renew_interval: 20s
  sqlite_busy_timeout: 5s
review:
  default_budget_cents: 1000
  currency: USD
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRuntime(path); err == nil {
		t.Fatal("LoadRuntime succeeded, want validation error")
	}
}

func TestLoadRuntimeRejectsUnsupportedBudgetCurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(`runtime:
  lease_ttl: 60s
  lease_renew_interval: 20s
  sqlite_busy_timeout: 5s
review:
  default_budget_cents: 1000
  currency: CNY
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRuntime(path); err == nil {
		t.Fatal("LoadRuntime succeeded, want unsupported currency error")
	}
}

func TestLoadRuntimeRejectsUnknownDefaultLLMTier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(`runtime:
  lease_ttl: 60s
  lease_renew_interval: 20s
  sqlite_busy_timeout: 5s
review:
  default_budget_cents: 1000
  currency: USD
  max_findings_per_unit: 5
llm:
  default_tier: strong
  tiers:
    economy:
      provider: fake
      model: fake-reviewer
      input_price_micros_per_million_tokens: 2000000
      output_price_micros_per_million_tokens: 4000000
      max_output_tokens: 1200
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRuntime(path); err == nil {
		t.Fatal("LoadRuntime succeeded, want unknown default tier error")
	}
}
