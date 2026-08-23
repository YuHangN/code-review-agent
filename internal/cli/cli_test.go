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
	"sync/atomic"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/cli"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

func TestExecuteRejectsRemovedDemoCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cli.Execute(context.Background(), []string{"demo"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("demo exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command: demo") {
		t.Fatalf("demo stderr = %q, want unknown command", stderr.String())
	}
}

func TestExecuteResumeCompletesPlannedRun(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	configPath := writeRuntimeConfig(t, "60s", "20s", "50ms")
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	store, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "run-resume", SourceURL: "https://example.test/pr/1", Provider: "fake", Repository: "acme/repo", ChangeNumber: 1, Status: domain.RunStatusPlanned, BudgetLimitMicros: 1_000_000, CreatedAt: now, UpdatedAt: now}
	unit := domain.ReviewUnit{ID: "unit-resume", RunID: run.ID, UnitKey: "main.go#1", FilePath: "main.go", StartLine: 1, EndLine: 1, DiffHunk: "@@ -0,0 +1 @@\n+safe\n", Risk: "low", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now}
	snapshot := domain.ChangeSnapshot{BaseSHA: "base", HeadSHA: "head", Diff: unit.DiffHunk, DiffSHA256: "hash", CreatedAt: now}
	if err := store.CreateRunWithSnapshot(ctx, run, []domain.ReviewUnit{unit}, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "report.md")
	var stdout, stderr bytes.Buffer
	if code := cli.Execute(ctx, []string{"resume", "--db", dbPath, "--config", configPath, "--output", outputPath, run.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status=reported") || !strings.Contains(stdout.String(), "reused=false") {
		t.Fatalf("resume stdout = %q", stdout.String())
	}
	store, err = sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gotRun, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	units, err := store.ListUnits(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != domain.RunStatusReported || len(units) != 1 || units[0].Status != domain.UnitStatusCompleted {
		t.Fatalf("resumed run = %#v, units = %#v", gotRun, units)
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
	outputPath := filepath.Join(t.TempDir(), "report.md")
	configPath := writeRuntimeConfig(t, "60s", "20s", "5s")
	var stdout, stderr bytes.Buffer
	code := cli.Execute(ctx, []string{
		"run", "--db", dbPath, "--config", configPath, "--budget-cents", "1000", "--output", outputPath,
		"https://github.com/acme/payments/pull/42",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "base_sha=base-sha") || !strings.Contains(stdout.String(), "head_sha=head-sha") {
		t.Fatalf("run stdout = %q, want pinned SHAs", stdout.String())
	}
	if !strings.Contains(stdout.String(), "status=reported") || !strings.Contains(stdout.String(), "report_path="+outputPath) {
		t.Fatalf("run stdout = %q, want completed report", stdout.String())
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
	if storedRun.Status != domain.RunStatusReported || len(units) != 1 || units[0].Status != domain.UnitStatusCompleted {
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
	outputPath := filepath.Join(t.TempDir(), "report.md")
	configPath := writeRuntimeConfig(t, "60s", "20s", "5s")
	var stdout, stderr bytes.Buffer
	if code := cli.Execute(ctx, []string{"run", "--db", dbPath, "--config", configPath, "--output", outputPath, "https://github.com/acme/payments/pull/42"}, &stdout, &stderr); code != 0 {
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

func TestExecuteRunPrintsRunIDBeforeRecoverableProviderFailure(t *testing.T) {
	var modelAvailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/payments/pulls/42":
			fmt.Fprint(writer, `{"base":{"sha":"base-sha"},"head":{"sha":"head-sha"}}`)
		case "/repos/acme/payments/compare/base-sha...head-sha":
			fmt.Fprint(writer, "diff --git a/main.go b/main.go\n@@ -0,0 +1 @@\n+changed\n")
		case "/v1/responses":
			writer.Header().Set("Content-Type", "application/json")
			if !modelAvailable.Load() {
				writer.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(writer, `{"error":{"message":"temporary model outage"}}`)
				return
			}
			fmt.Fprint(writer, `{"id":"resp-2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"findings\":[]}"}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`)
		default:
			t.Fatalf("unexpected request path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("GITHUB_API_BASE_URL", server.URL)
	t.Setenv("OPENAI_API_BASE_URL", server.URL)
	t.Setenv("OPENAI_API_KEY", "test-openai-key")

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "review.db")
	configPath := writeOpenAIRuntimeConfig(t)
	var stdout, stderr bytes.Buffer
	code := cli.Execute(ctx, []string{"run", "--db", dbPath, "--config", configPath, "--output", filepath.Join(t.TempDir(), "report.md"), "https://github.com/acme/payments/pull/42"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "temporary model outage") {
		t.Fatalf("run stderr = %q", stderr.String())
	}
	firstLine := strings.Split(stdout.String(), "\n")[0]
	if !strings.HasPrefix(firstLine, "run_id=run-") {
		t.Fatalf("run stdout = %q, want resumable run ID", stdout.String())
	}
	runID := strings.TrimPrefix(firstLine, "run_id=")
	store, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	units, err := store.ListUnits(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunStatusReviewing || len(units) != 1 || units[0].Status != domain.UnitStatusFailedRecoverable {
		t.Fatalf("failed run = %#v, units = %#v", run, units)
	}
	modelAvailable.Store(true)
	stdout.Reset()
	stderr.Reset()
	outputPath := filepath.Join(t.TempDir(), "resumed.md")
	if code := cli.Execute(ctx, []string{"resume", "--db", dbPath, "--config", configPath, "--output", outputPath, runID}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status=reported") || !strings.Contains(stdout.String(), "reused=false") {
		t.Fatalf("resume stdout = %q", stdout.String())
	}
	units, err = store.ListUnits(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].Status != domain.UnitStatusCompleted || units[0].Attempt != 2 {
		t.Fatalf("resumed units = %#v", units)
	}
	summary, err := store.BudgetSummary(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ReservedMicros != 0 || summary.ActualMicros != 17 {
		t.Fatalf("resumed budget = %#v, want only successful call cost", summary)
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
		"  request_timeout: 90s\n" +
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

func writeOpenAIRuntimeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	content := "runtime:\n" +
		"  lease_ttl: 60s\n" +
		"  lease_renew_interval: 20s\n" +
		"  sqlite_busy_timeout: 5s\n" +
		"review:\n" +
		"  default_budget_cents: 1000\n" +
		"  currency: USD\n" +
		"  max_findings_per_unit: 5\n" +
		"llm:\n" +
		"  request_timeout: 5s\n" +
		"  default_tier: economy\n" +
		"  tiers:\n" +
		"    economy:\n" +
		"      provider: openai\n" +
		"      model: gpt-test\n" +
		"      input_price_micros_per_million_tokens: 750000\n" +
		"      output_price_micros_per_million_tokens: 4500000\n" +
		"      max_output_tokens: 100\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
