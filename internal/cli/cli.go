package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/config"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/llm"
	"github.com/YuHangN/code-review-agent/internal/planner"
	"github.com/YuHangN/code-review-agent/internal/report"
	"github.com/YuHangN/code-review-agent/internal/review"
	"github.com/YuHangN/code-review-agent/internal/scm"
	"github.com/YuHangN/code-review-agent/internal/security"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
	"github.com/YuHangN/code-review-agent/internal/verifier"
	"github.com/YuHangN/code-review-agent/internal/workflow"
)

const (
	defaultDBPath     = "review-agent.db"
	defaultConfigPath = "config/runtime.yaml"
	defaultGitHubAPI  = "https://api.github.com"
	microsPerCent     = int64(10_000)
)

// Execute 解析一个 CLI 子命令，并返回进程退出码。
// 输出 writer 由调用方传入，方便测试命令行为而无需真实终端。
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "run":
		return executeRun(ctx, args[1:], stdout, stderr)
	case "demo":
		return executeDemo(ctx, args[1:], stdout, stderr)
	case "status":
		return executeStatus(ctx, args[1:], stdout, stderr)
	case "resume":
		return executeResume(ctx, args[1:], stdout, stderr)
	case "trace":
		return executeTrace(ctx, args[1:], stdout, stderr)
	case "report":
		return executeReport(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// executeRun 拉取并脱敏固定 Snapshot，再生成 Unit 并推进到 planned checkpoint。
func executeRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", defaultDBPath, "SQLite database path")
	configPath := flags.String("config", defaultConfigPath, "runtime config path")
	budgetCents := flags.Int64("budget-cents", 0, "override total budget in cents")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	budgetOverridden := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "budget-cents" {
			budgetOverridden = true
		}
	})
	if len(flags.Args()) != 1 {
		fmt.Fprintln(stderr, "run requires a GitHub pull request URL")
		return 2
	}
	if budgetOverridden && !validBudgetCents(*budgetCents) {
		fmt.Fprintln(stderr, "budget-cents must be a positive integer within range")
		return 2
	}

	ref, err := scm.ParseGitHubPullRequestURL(flags.Args()[0])
	if err != nil {
		fmt.Fprintf(stderr, "parse pull request URL: %v\n", err)
		return 2
	}
	store, runtimeConfig, err := openStore(ctx, *dbPath, *configPath)
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer store.Close()

	effectiveBudgetCents := runtimeConfig.DefaultBudgetCents
	if budgetOverridden {
		effectiveBudgetCents = *budgetCents
	}
	if !validBudgetCents(effectiveBudgetCents) {
		fmt.Fprintln(stderr, "configured default budget is outside the supported range")
		return 1
	}

	apiBaseURL := os.Getenv("GITHUB_API_BASE_URL")
	if apiBaseURL == "" {
		apiBaseURL = defaultGitHubAPI
	}
	adapter, err := scm.NewGitHubAdapter(nil, apiBaseURL, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		fmt.Fprintf(stderr, "create GitHub adapter: %v\n", err)
		return 1
	}
	snapshot, err := adapter.Fetch(ctx, ref)
	if err != nil {
		fmt.Fprintf(stderr, "fetch pull request: %v\n", err)
		return 1
	}
	sanitized := security.NewSanitizer().SanitizeSnapshot(snapshot)
	snapshot = sanitized.Snapshot

	runID, err := newRunID()
	if err != nil {
		fmt.Fprintf(stderr, "create run ID: %v\n", err)
		return 1
	}
	now := time.Now().UTC()
	snapshot.CreatedAt = now
	run := domain.Run{
		ID:                runID,
		SourceURL:         flags.Args()[0],
		Provider:          "github",
		Repository:        ref.Owner + "/" + ref.Repository,
		ChangeNumber:      ref.Number,
		Status:            domain.RunStatusFetched,
		BudgetLimitMicros: effectiveBudgetCents * microsPerCent,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	service := workflow.NewService(store)
	if err := service.StartFetched(ctx, workflow.FetchedRunRequest{Run: run, Snapshot: snapshot}); err != nil {
		fmt.Fprintf(stderr, "create fetched run: %v\n", err)
		return 1
	}
	units := planner.New().Plan(planner.Request{
		RunID:   run.ID,
		HeadSHA: snapshot.HeadSHA,
		Diff:    snapshot.Diff,
		Now:     now,
	})
	if err := service.SavePlan(ctx, run.ID, units, now); err != nil {
		fmt.Fprintf(stderr, "save review plan: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run_id=%s\nbase_sha=%s\nhead_sha=%s\nunits=%d\nredactions=%d\nexcluded_files=%d\n", run.ID, snapshot.BaseSHA, snapshot.HeadSHA, len(units), len(sanitized.Redactions), len(sanitized.ExcludedFiles))
	return 0
}

// validBudgetCents 确保预算为正数，并且转换成微美元后不会溢出。
func validBudgetCents(value int64) bool {
	return value > 0 && value <= math.MaxInt64/microsPerCent
}

// executeDemo 使用 Fake Provider 跑通完整的离线 Review 链路，不访问网络。
func executeDemo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", defaultDBPath, "SQLite database path")
	configPath := flags.String("config", defaultConfigPath, "runtime config path")
	outputPath := flags.String("output", filepath.Join("out", "demo-report.md"), "Markdown report output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(flags.Args()) != 0 {
		fmt.Fprintln(stderr, "demo does not accept positional arguments")
		return 2
	}

	store, runtime, err := openStore(ctx, *dbPath, *configPath)
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
		Status:            domain.RunStatusFetched,
		BudgetLimitMicros: 1_000_000,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	unit := domain.ReviewUnit{
		ID: "unit-demo", RunID: run.ID, UnitKey: "internal/cache/cache.go#1-9",
		FilePath: "internal/cache/cache.go", StartLine: 1, EndLine: 9,
		DiffHunk: "@@ -0,0 +1,9 @@\n+package cache\n+\n+api_key := \"<REDACTED:API_KEY:1>\"\n+\n+var cache = map[string]string{}\n+\n+func Update(key, value string) {\n+\tgo func() { cache[key] = value }()\n+}\n",
		Risk:     "high", Status: domain.UnitStatusPending, CreatedAt: now, UpdatedAt: now,
	}
	diffHash := sha256.Sum256([]byte(unit.DiffHunk))
	snapshot := domain.ChangeSnapshot{
		BaseSHA: "demo-base-sha", HeadSHA: "demo-head-sha", Diff: unit.DiffHunk,
		DiffSHA256: fmt.Sprintf("%x", diffHash[:]), CreatedAt: now,
	}
	service := workflow.NewService(store)
	if err := service.StartFetched(ctx, workflow.FetchedRunRequest{Run: run, Snapshot: snapshot}); err != nil {
		fmt.Fprintf(stderr, "create demo run: %v\n", err)
		return 1
	}
	if err := service.SavePlan(ctx, run.ID, []domain.ReviewUnit{unit}, now); err != nil {
		fmt.Fprintf(stderr, "save demo plan: %v\n", err)
		return 1
	}
	provider := &llm.FakeProvider{Response: llm.Response{
		Content: `{"findings":[{"category":"security","severity":"high","file":"internal/cache/cache.go","line":3,"title":"配置中包含硬编码 API Key","explanation":"凭据不应直接写入源码","evidence":["新增 api_key 赋值"],"suggestion":"改为从环境变量或密钥服务读取"},{"category":"concurrency","severity":"high","file":"internal/cache/cache.go","line":8,"title":"共享 map 存在并发访问风险","explanation":"新增 goroutine 在没有同步保护的情况下写入包级 map，可能与其他读写并发执行","evidence":["第 5 行声明包级 map，第 8 行从 goroutine 写入"],"suggestion":"使用 mutex 保护共享 map，或改用并发安全的数据结构"}]}`,
		Usage:   &llm.TokenUsage{InputTokens: 120, OutputTokens: 180},
	}}
	gateway := llm.NewGateway(
		budget.NewManager(store), llm.ByteUpperBoundCounter{},
		map[string]llm.Provider{"fake": provider}, runtime.LLMTiers,
	)
	reviewer := review.NewReviewer(gateway, runtime.DefaultLLMTier, runtime.MaxFindingsPerUnit)
	owner, err := newExecutorID()
	if err != nil {
		fmt.Fprintf(stderr, "create executor ID: %v\n", err)
		return 1
	}
	executor := review.NewExecutor(store, reviewer, "llm_review", owner)
	runner := workflow.NewRunner(service, executor)
	result, err := runner.Run(ctx, workflow.RunRequest{
		RunID: run.ID, Owner: owner,
		Lease: workflow.LeaseSettings{TTL: runtime.LeaseTTL, RenewInterval: runtime.LeaseRenewInterval},
	})
	if err != nil {
		fmt.Fprintf(stderr, "run demo review: %v\n", err)
		return 1
	}
	candidates, err := store.ListCandidateFindings(ctx, run.ID)
	if err != nil || len(candidates) == 0 {
		fmt.Fprintf(stderr, "read demo finding: %v\n", err)
		return 1
	}
	aggregation, err := verifier.NewAggregator(store, verifier.NewDefault()).Aggregate(ctx, run.ID, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "aggregate demo findings: %v\n", err)
		return 1
	}
	reportResult, err := report.NewGenerator(store).Generate(ctx, report.GenerateRequest{RunID: run.ID, OutputPath: *outputPath, Now: time.Now().UTC()})
	if err != nil {
		fmt.Fprintf(stderr, "generate demo report: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run_id=%s\nstatus=%s\ncompleted=%d\nconfirmed=%d\nadvisory=%d\ntrace_id=%s\nreport_path=%s\n", run.ID, domain.RunStatusReported, result.Completed, aggregation.Confirmed, aggregation.Advisory, candidates[0].TraceID, reportResult.Report.OutputPath)
	return 0
}

// executeReport 为 aggregating Run 生成报告，或从 reported checkpoint 恢复报告文件。
func executeReport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", defaultDBPath, "SQLite database path")
	configPath := flags.String("config", defaultConfigPath, "runtime config path")
	outputPath := flags.String("output", "", "Markdown report output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(flags.Args()) != 1 {
		fmt.Fprintln(stderr, "report requires a run ID")
		return 2
	}
	runID := flags.Args()[0]
	if *outputPath == "" {
		*outputPath = filepath.Join("out", runID+".md")
	}
	store, _, err := openStore(ctx, *dbPath, *configPath)
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer store.Close()
	result, err := report.NewGenerator(store).Generate(ctx, report.GenerateRequest{RunID: runID, OutputPath: *outputPath, Now: time.Now().UTC()})
	if err != nil {
		fmt.Fprintf(stderr, "generate report: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run_id=%s\nstatus=%s\nreport_path=%s\ncontent_sha256=%s\nreused=%t\n", runID, domain.RunStatusReported, result.Report.OutputPath, result.Report.ContentSHA256, result.Reused)
	return 0
}

// executeTrace 展示一条 finding 级证据链中的 diff、Prompt、模型回复和候选问题。
func executeTrace(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dbPath, configPath, rest, ok := parseOptions("trace", args, stderr)
	if !ok {
		return 2
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "trace requires a trace ID")
		return 2
	}
	store, _, err := openStore(ctx, dbPath, configPath)
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer store.Close()

	trace, err := store.GetReviewTrace(ctx, rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "get trace: %v\n", err)
		return 1
	}
	unit, err := store.GetReviewUnit(ctx, trace.UnitID)
	if err != nil {
		fmt.Fprintf(stderr, "get trace unit: %v\n", err)
		return 1
	}
	findings, err := store.ListCandidateFindings(ctx, trace.RunID)
	if err != nil {
		fmt.Fprintf(stderr, "list trace findings: %v\n", err)
		return 1
	}
	verified, err := store.ListVerifiedFindings(ctx, trace.RunID)
	if err != nil {
		fmt.Fprintf(stderr, "list verified findings: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "trace_id=%s\nrun_id=%s\nunit_id=%s\ndetector=%s\nstatus=%s\n", trace.ID, trace.RunID, trace.UnitID, trace.Detector, trace.Status)
	fmt.Fprintf(stdout, "diff:\n%s\nprompt:\n%s\nresponse:\n%s\n", unit.DiffHunk, trace.Prompt, trace.Response)
	if trace.ErrorMessage != "" {
		fmt.Fprintf(stdout, "error=%s\n", trace.ErrorMessage)
	}
	for _, finding := range findings {
		if finding.TraceID != trace.ID {
			continue
		}
		fmt.Fprintf(stdout, "candidate_id=%s\nfile=%s\nline=%d\nseverity=%s\ntitle=%s\n", finding.ID, finding.File, finding.Line, finding.Severity, finding.Title)
		fmt.Fprintf(stdout, "explanation=%s\nsuggestion=%s\n", finding.Explanation, finding.Suggestion)
		for _, evidence := range finding.Evidence {
			fmt.Fprintf(stdout, "evidence=%s\n", evidence)
		}
	}
	for _, finding := range verified {
		if finding.TraceID != trace.ID {
			continue
		}
		fmt.Fprintf(stdout, "finding_id=%s\nconfidence=%s\nverification_source=%s\nverification_reason=%s\n", finding.ID, finding.Confidence, finding.VerificationSource, finding.VerificationReason)
	}
	return 0
}

// executeStatus 输出已持久化 Run 及其 Unit 状态汇总。
func executeStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dbPath, configPath, rest, ok := parseOptions("status", args, stderr)
	if !ok {
		return 2
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "status requires a run ID")
		return 2
	}

	store, _, err := openStore(ctx, dbPath, configPath)
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
	budgetSummary, err := budget.NewManager(store).Summary(ctx, run.ID)
	if err != nil {
		fmt.Fprintf(stderr, "summarize budget: %v\n", err)
		return 1
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
	fmt.Fprintf(stdout, "budget_limit_micros=%d\nbudget_reserved_micros=%d\nbudget_actual_micros=%d\nbudget_committed_micros=%d\n", run.BudgetLimitMicros, budgetSummary.ReservedMicros, budgetSummary.ActualMicros, budgetSummary.CommittedMicros)
	return 0
}

// executeResume 为当前 CLI 领取 Run，并输出仍需处理的 Unit。
func executeResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dbPath, configPath, rest, ok := parseOptions("resume", args, stderr)
	if !ok {
		return 2
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "resume requires a run ID")
		return 2
	}

	store, runtime, err := openStore(ctx, dbPath, configPath)
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
	result, err := workflow.NewService(store).Resume(ctx, rest[0], owner, time.Now().UTC(), runtime.LeaseTTL)
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
	randomSuffix, err := randomSuffix()
	if err != nil {
		return "", err
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	return fmt.Sprintf("cli-%s-%d-%s", hostname, os.Getpid(), randomSuffix), nil
}

// newRunID 为持久化 Run 生成不会与其他命令冲突的标识。
func newRunID() (string, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return "", err
	}
	return "run-" + suffix, nil
}

func randomSuffix() (string, error) {
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(randomBytes[:]), nil
}

// parseOptions 解析所有子命令共用的数据库与运行时配置参数。
func parseOptions(name string, args []string, stderr io.Writer) (string, string, []string, bool) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", defaultDBPath, "SQLite database path")
	configPath := flags.String("config", defaultConfigPath, "runtime config path")
	if err := flags.Parse(args); err != nil {
		return "", "", nil, false
	}
	return *dbPath, *configPath, flags.Args(), true
}

// openStore 读取运行时配置，并以其中的 SQLite 参数打开数据库。
func openStore(ctx context.Context, dbPath, configPath string) (*sqlite.Store, config.Runtime, error) {
	runtime, err := config.LoadRuntime(configPath)
	if err != nil {
		return nil, config.Runtime{}, fmt.Errorf("load runtime config: %w", err)
	}
	store, err := sqlite.Open(ctx, dbPath, sqlite.Options{BusyTimeout: runtime.SQLiteBusyTimeout})
	if err != nil {
		return nil, config.Runtime{}, err
	}
	return store, runtime, nil
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: review-agent <run|demo|status|resume|trace|report> [--db path] [--config path] [id]")
}
