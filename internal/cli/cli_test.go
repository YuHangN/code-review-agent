package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/cli"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

func TestExecuteDemoStatusAndResume(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	configPath := writeRuntimeConfig(t, "60s", "20s", "5s")

	var stdout, stderr bytes.Buffer
	if code := cli.Execute(ctx, []string{"demo", "--db", dbPath, "--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("demo exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "run_id=demo-run") {
		t.Fatalf("demo stdout = %q, want run ID", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("demo stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	if code := cli.Execute(ctx, []string{"status", "--db", dbPath, "--config", configPath, "demo-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit code = %d, stderr = %s", code, stderr.String())
	}
	for _, want := range []string{"status=created", "units=5", "pending=1", "running=1", "failed_recoverable=1", "completed=1", "skipped_budget=1"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout = %q, want %q", stdout.String(), want)
		}
	}

	stdout.Reset()
	if code := cli.Execute(ctx, []string{"resume", "--db", dbPath, "--config", configPath, "demo-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit code = %d, stderr = %s", code, stderr.String())
	}
	for _, want := range []string{"unit-pending", "unit-retry", "unit-running"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("resume stdout = %q, want %q", stdout.String(), want)
		}
	}
	for _, forbidden := range []string{"unit-completed", "unit-budget"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("resume stdout = %q, must not contain %q", stdout.String(), forbidden)
		}
	}
}

func TestExecuteResumeRejectsSecondCLIProcessWhileLeaseIsValid(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	configPath := writeRuntimeConfig(t, "60s", "20s", "5s")

	var stdout, stderr bytes.Buffer
	if code := cli.Execute(ctx, []string{"demo", "--db", dbPath, "--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("demo exit code = %d, stderr = %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := cli.Execute(ctx, []string{"resume", "--db", dbPath, "--config", configPath, "demo-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("first resume exit code = %d, stderr = %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := cli.Execute(ctx, []string{"resume", "--db", dbPath, "--config", configPath, "demo-run"}, &stdout, &stderr); code != 1 {
		t.Fatalf("second resume exit code = %d, want 1; stdout = %s; stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "run lease is held by another owner") {
		t.Fatalf("second resume stderr = %q, want lease conflict", stderr.String())
	}
}

func TestExecuteResumeUsesConfiguredLeaseTTL(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	configPath := writeRuntimeConfig(t, "2h", "30m", "50ms")

	var stdout, stderr bytes.Buffer
	if code := cli.Execute(ctx, []string{"demo", "--db", dbPath, "--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("demo exit code = %d, stderr = %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	startedAt := time.Now().UTC()
	if code := cli.Execute(ctx, []string{"resume", "--db", dbPath, "--config", configPath, "demo-run"}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit code = %d, stderr = %s", code, stderr.String())
	}

	store, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	run, err := store.GetRun(ctx, "demo-run")
	if err != nil {
		t.Fatal(err)
	}
	if remaining := run.LeaseExpiresAt.Sub(startedAt); remaining < time.Hour+59*time.Minute || remaining > 2*time.Hour+time.Minute {
		t.Fatalf("lease duration = %s, want about 2h", remaining)
	}
}

func writeRuntimeConfig(t *testing.T, ttl, interval, busyTimeout string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	content := "runtime:\n" +
		"  lease_ttl: " + ttl + "\n" +
		"  lease_renew_interval: " + interval + "\n" +
		"  sqlite_busy_timeout: " + busyTimeout + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
