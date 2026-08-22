package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/cli"
	"github.com/YuHangN/code-review-agent/internal/domain"
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
	for _, want := range []string{"status=created", "units=5", "pending=1", "running=1", "failed_recoverable=1", "completed=1", "skipped_budget=1", "budget_limit_micros=1000000", "budget_reserved_micros=0", "budget_actual_micros=0", "budget_committed_micros=0"} {
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

func TestExecuteRunFetchesAndPersistsGitHubSnapshot(t *testing.T) {
	const secret = "sk-live-1234567890abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/payments/pulls/42":
			if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Fatalf("authorization = %q, want bearer token", got)
			}
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"base":{"sha":"base-sha"},"head":{"sha":"head-sha"}}`)
		case "/repos/acme/payments/compare/base-sha...head-sha":
			fmt.Fprint(writer, "diff --git a/config.yaml b/config.yaml\n+api_key: \""+secret+"\"\n")
		default:
			t.Fatalf("unexpected request path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("GITHUB_API_BASE_URL", server.URL)
	t.Setenv("GITHUB_TOKEN", "test-token")

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	configPath := writeRuntimeConfig(t, "60s", "20s", "5s")
	var stdout, stderr bytes.Buffer
	code := cli.Execute(ctx, []string{
		"run", "--db", dbPath, "--config", configPath, "--budget-cents", "1000",
		"https://github.com/acme/payments/pull/42",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "base_sha=base-sha") || !strings.Contains(stdout.String(), "head_sha=head-sha") {
		t.Fatalf("run stdout = %q, want pinned SHAs", stdout.String())
	}

	store, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runID := strings.TrimPrefix(strings.Split(stdout.String(), "\n")[0], "run_id=")
	snapshot, err := store.GetSnapshot(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseSHA != "base-sha" || snapshot.HeadSHA != "head-sha" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if strings.Contains(snapshot.Diff, secret) {
		t.Fatalf("persisted snapshot contains secret: %q", snapshot.Diff)
	}
	if !strings.Contains(snapshot.Diff, "<REDACTED:API_KEY:1>") {
		t.Fatalf("persisted snapshot = %q, want redaction placeholder", snapshot.Diff)
	}
	storedRun, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	units, err := store.ListUnits(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.Status != domain.RunStatusPlanned || len(units) != 1 || units[0].Status != domain.UnitStatusPending {
		t.Fatalf("run status = %q, units = %#v", storedRun.Status, units)
	}
	if storedRun.BudgetLimitMicros != 10_000_000 {
		t.Fatalf("budget limit = %d, want CLI override 10000000", storedRun.BudgetLimitMicros)
	}
}

func TestExecuteRunUsesConfiguredDefaultBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/payments/pulls/42":
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"base":{"sha":"base-sha"},"head":{"sha":"head-sha"}}`)
		case "/repos/acme/payments/compare/base-sha...head-sha":
			fmt.Fprint(writer, "diff --git a/main.go b/main.go\n@@ -1 +1 @@\n+safe\n")
		default:
			t.Fatalf("unexpected request path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("GITHUB_API_BASE_URL", server.URL)

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	configPath := writeRuntimeConfig(t, "60s", "20s", "5s")
	var stdout, stderr bytes.Buffer
	if code := cli.Execute(ctx, []string{"run", "--db", dbPath, "--config", configPath, "https://github.com/acme/payments/pull/42"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit code = %d, stderr = %s", code, stderr.String())
	}
	runID := strings.TrimPrefix(strings.Split(stdout.String(), "\n")[0], "run_id=")
	store, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.BudgetLimitMicros != 7_500_000 {
		t.Fatalf("budget limit = %d, want configured default 7500000", run.BudgetLimitMicros)
	}
}

func writeRuntimeConfig(t *testing.T, ttl, interval, busyTimeout string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	content := "runtime:\n" +
		"  lease_ttl: " + ttl + "\n" +
		"  lease_renew_interval: " + interval + "\n" +
		"  sqlite_busy_timeout: " + busyTimeout + "\n" +
		"review:\n" +
		"  default_budget_cents: 750\n" +
		"  currency: USD\n" +
		"  max_findings_per_unit: 5\n" +
		"llm:\n" +
		"  default_tier: economy\n" +
		"  tiers:\n" +
		"    economy:\n" +
		"      provider: fake\n" +
		"      model: fake-reviewer\n" +
		"      input_price_micros_per_million_tokens: 2000000\n" +
		"      output_price_micros_per_million_tokens: 4000000\n" +
		"      max_output_tokens: 1200\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
