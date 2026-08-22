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
}

func TestLoadRuntimeRejectsRenewIntervalAtOrAfterTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	if err := os.WriteFile(path, []byte(`runtime:
  lease_ttl: 20s
  lease_renew_interval: 20s
  sqlite_busy_timeout: 5s
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRuntime(path); err == nil {
		t.Fatal("LoadRuntime succeeded, want validation error")
	}
}
