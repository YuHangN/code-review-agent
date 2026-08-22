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
