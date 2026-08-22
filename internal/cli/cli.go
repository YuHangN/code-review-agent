package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
	"github.com/YuHangN/code-review-agent/internal/workflow"
)

const defaultDBPath = "review-agent.db"

// Execute 解析一个 CLI 子命令，并返回进程退出码。
// 输出 writer 由调用方传入，方便测试命令行为而无需真实终端。
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "demo":
		return executeDemo(ctx, args[1:], stdout, stderr)
	case "status":
		return executeStatus(ctx, args[1:], stdout, stderr)
	case "resume":
		return executeResume(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// executeDemo 写入一个固定的离线 Run，包含不同 checkpoint 状态的 Unit。
func executeDemo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dbPath, rest, ok := parseDB("demo", args, stderr)
	if !ok {
		return 2
	}
	if len(rest) != 0 {
		fmt.Fprintln(stderr, "demo does not accept positional arguments")
		return 2
	}

	store, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer store.Close()

	now := time.Now().UTC()
	run := domain.Run{
		ID:                "demo-run",
		SourceURL:         "https://example.test/acme/demo/changes/42",
		Provider:          "fake",
		Repository:        "acme/demo",
		ChangeNumber:      42,
		Status:            domain.RunStatusCreated,
		BudgetLimitMicros: 1_000_000,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := workflow.NewService(store).Start(ctx, workflow.StartRequest{Run: run, Units: demoUnits(run.ID, now)}); err != nil {
		fmt.Fprintf(stderr, "create demo run: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run_id=%s\n", run.ID)
	return 0
}

// executeStatus 输出已持久化 Run 及其 Unit 状态汇总。
func executeStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dbPath, rest, ok := parseDB("status", args, stderr)
	if !ok {
		return 2
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "status requires a run ID")
		return 2
	}

	store, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer store.Close()

	run, err := store.GetRun(ctx, rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "get run: %v\n", err)
		return 1
	}
	units, err := store.ListUnits(ctx, run.ID)
	if err != nil {
		fmt.Fprintf(stderr, "list units: %v\n", err)
		return 1
	}
	counts := make(map[domain.UnitStatus]int)
	for _, unit := range units {
		counts[unit.Status]++
	}
	fmt.Fprintf(stdout, "run_id=%s\nstatus=%s\nunits=%d\n", run.ID, run.Status, len(units))
	for _, status := range []domain.UnitStatus{
		domain.UnitStatusPending,
		domain.UnitStatusRunning,
		domain.UnitStatusFailedRecoverable,
		domain.UnitStatusCompleted,
		domain.UnitStatusSkippedBudget,
	} {
		fmt.Fprintf(stdout, "%s=%d\n", status, counts[status])
	}
	return 0
}

// executeResume 为当前 CLI 领取 Run，并输出仍需处理的 Unit。
func executeResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dbPath, rest, ok := parseDB("resume", args, stderr)
	if !ok {
		return 2
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "resume requires a run ID")
		return 2
	}

	store, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer store.Close()

	owner, err := newExecutorID()
	if err != nil {
		fmt.Fprintf(stderr, "create executor ID: %v\n", err)
		return 1
	}
	result, err := workflow.NewService(store).Resume(ctx, rest[0], owner, time.Now().UTC(), time.Minute)
	if err != nil {
		fmt.Fprintf(stderr, "resume run: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run_id=%s\nresumable_units=%d\n", result.Run.ID, len(result.PendingUnits))
	for _, unit := range result.PendingUnits {
		fmt.Fprintln(stdout, unit.ID)
	}
	return 0
}

// newExecutorID 为一次 CLI 进程生成唯一的 lease owner。
// 主机名和 PID 便于排查，随机后缀保证不同进程不会被误判为同一执行者。
func newExecutorID() (string, error) {
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	return fmt.Sprintf("cli-%s-%d-%s", hostname, os.Getpid(), hex.EncodeToString(randomBytes[:])), nil
}

// parseDB 解析所有子命令共用的 --db 参数，并保留位置参数给各命令处理。
func parseDB(name string, args []string, stderr io.Writer) (string, []string, bool) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", defaultDBPath, "SQLite database path")
	if err := flags.Parse(args); err != nil {
		return "", nil, false
	}
	return *dbPath, flags.Args(), true
}

// demoUnits 为离线 Demo 创建各种与恢复相关状态的 Unit。
func demoUnits(runID string, now time.Time) []domain.ReviewUnit {
	return []domain.ReviewUnit{
		newDemoUnit(runID, "unit-pending", domain.UnitStatusPending, now),
		newDemoUnit(runID, "unit-running", domain.UnitStatusRunning, now),
		newDemoUnit(runID, "unit-retry", domain.UnitStatusFailedRecoverable, now),
		newDemoUnit(runID, "unit-completed", domain.UnitStatusCompleted, now),
		newDemoUnit(runID, "unit-budget", domain.UnitStatusSkippedBudget, now),
	}
}

func newDemoUnit(runID, id string, status domain.UnitStatus, now time.Time) domain.ReviewUnit {
	return domain.ReviewUnit{
		ID:        id,
		RunID:     runID,
		UnitKey:   id,
		FilePath:  "fixtures/demo.go",
		Risk:      "medium",
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: review-agent <demo|status|resume> [--db path] [run-id]")
}
