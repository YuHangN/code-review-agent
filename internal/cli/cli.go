package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	reviewtools "github.com/YuHangN/code-review-agent/internal/tools"
	"github.com/YuHangN/code-review-agent/internal/verifier"
	"github.com/YuHangN/code-review-agent/internal/workflow"
)

const (
	defaultDBPath     = "review-agent.db"
	defaultConfigPath = "config/runtime.yaml"
	defaultGitHubAPI  = "https://api.github.com"
	defaultOpenAIAPI  = "https://api.openai.com"
	defaultToolsPath  = "config/tools.yaml"
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

// executeRun 创建固定 Snapshot 和 Review Plan，再通过统一执行引擎生成报告。
func executeRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", defaultDBPath, "SQLite database path")
	configPath := flags.String("config", defaultConfigPath, "runtime config path")
	budgetCents := flags.Int64("budget-cents", 0, "override total budget in cents")
	outputPath := flags.String("output", "", "Markdown report output path")
	toolsPath := flags.String("tools-config", configuredToolsPath(), "tools config path")
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
	providers, err := providersForRuntime(runtimeConfig)
	if err != nil {
		fmt.Fprintf(stderr, "configure LLM providers: %v\n", err)
		return 1
	}
	toolConfig, err := reviewtools.LoadConfig(*toolsPath)
	if err != nil {
		fmt.Fprintf(stderr, "load tools config: %v\n", err)
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

	runID, err := newRunID()
	if err != nil {
		fmt.Fprintf(stderr, "create run ID: %v\n", err)
		return 1
	}
	if *outputPath == "" {
		*outputPath = filepath.Join("out", runID+".md")
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID:                runID,
		SourceURL:         flags.Args()[0],
		Provider:          "github",
		Repository:        ref.Owner + "/" + ref.Repository,
		ChangeNumber:      ref.Number,
		Status:            domain.RunStatusCreated,
		BudgetLimitMicros: effectiveBudgetCents * microsPerCent,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	service := workflow.NewService(store)
	if err := service.Start(ctx, workflow.StartRequest{Run: run}); err != nil {
		fmt.Fprintf(stderr, "create run: %v\n", err)
		return 1
	}
	// Run 一经持久化就立即输出 ID；即使首次 Fetch 失败也能通过 resume 继续。
	fmt.Fprintf(stdout, "run_id=%s\n", run.ID)
	if err := service.BeginFetch(ctx, run.ID, time.Now().UTC()); err != nil {
		fmt.Fprintf(stderr, "begin fetch: %v\n", err)
		return 1
	}
	snapshot, err := adapter.Fetch(ctx, ref)
	if err != nil {
		fmt.Fprintf(stderr, "fetch pull request: %v\n", err)
		return 1
	}
	sanitized := security.NewSanitizer().SanitizeSnapshot(snapshot)
	snapshot = sanitized.Snapshot
	snapshot.CreatedAt = time.Now().UTC()
	if err := service.CompleteFetch(ctx, run.ID, snapshot, snapshot.CreatedAt); err != nil {
		fmt.Fprintf(stderr, "save fetched snapshot: %v\n", err)
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
	fmt.Fprintf(stdout, "base_sha=%s\nhead_sha=%s\nunits=%d\nredactions=%d\nexcluded_files=%d\n", snapshot.BaseSHA, snapshot.HeadSHA, len(units), len(sanitized.Redactions), len(sanitized.ExcludedFiles))
	owner, err := newExecutorID()
	if err != nil {
		fmt.Fprintf(stderr, "create executor ID: %v\n", err)
		return 1
	}
	registry, err := newToolRegistry(toolConfig, adapter, ref, snapshot)
	if err != nil {
		fmt.Fprintf(stderr, "configure review tools: %v\n", err)
		return 1
	}
	engine := newExecutionEngine(store, runtimeConfig, owner, providers, runtimeConfig.LLMTiers, registry, toolConfig.Agent)
	result, err := engine.Execute(ctx, workflow.EngineRequest{
		RunID: run.ID, Owner: owner, OutputPath: *outputPath,
		Lease: workflow.LeaseSettings{TTL: runtimeConfig.LeaseTTL, RenewInterval: runtimeConfig.LeaseRenewInterval},
	})
	if err != nil {
		fmt.Fprintf(stderr, "execute review: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "status=%s\nreport_path=%s\n", result.Status, result.Report.Report.OutputPath)
	return 0
}

// validBudgetCents 确保预算为正数，并且转换成微美元后不会溢出。
func validBudgetCents(value int64) bool {
	return value > 0 && value <= math.MaxInt64/microsPerCent
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
	agentSteps, err := store.ListAgentSteps(ctx, trace.UnitID)
	if err != nil {
		fmt.Fprintf(stderr, "list trace agent steps: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "trace_id=%s\nrun_id=%s\nunit_id=%s\ndetector=%s\nstatus=%s\n", trace.ID, trace.RunID, trace.UnitID, trace.Detector, trace.Status)
	fmt.Fprintf(stdout, "diff:\n%s\nprompt:\n%s\nresponse:\n%s\n", unit.DiffHunk, trace.Prompt, trace.Response)
	for _, step := range agentSteps {
		fmt.Fprintf(stdout, "agent_round=%d\nmodel_call_id=%s\nagent_prompt:\n%s\nagent_response:\n%s\n", step.Round, step.ModelCallID, step.Prompt, step.Response)
		for _, call := range step.ToolCalls {
			fmt.Fprintf(stdout, "tool_call id=%s name=%s arguments=%s\n", call.ID, call.Name, call.Arguments)
		}
		for _, result := range step.ToolResults {
			fmt.Fprintf(stdout, "tool_result call_id=%s name=%s\n", result.CallID, result.Name)
			if result.Error != "" {
				fmt.Fprintf(stdout, "tool_error=%s\n", result.Error)
			} else {
				fmt.Fprintf(stdout, "tool_content=%s\n", result.Content)
			}
		}
	}
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

// executeResume 根据持久化状态继续执行，不重做已完成的阶段。
func executeResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", defaultDBPath, "SQLite database path")
	configPath := flags.String("config", defaultConfigPath, "runtime config path")
	outputPath := flags.String("output", "", "Markdown report output path")
	toolsPath := flags.String("tools-config", configuredToolsPath(), "tools config path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(flags.Args()) != 1 {
		fmt.Fprintln(stderr, "resume requires a run ID")
		return 2
	}
	runID := flags.Args()[0]
	if *outputPath == "" {
		*outputPath = filepath.Join("out", runID+".md")
	}

	store, runtime, err := openStore(ctx, *dbPath, *configPath)
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer store.Close()
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		fmt.Fprintf(stderr, "get run: %v\n", err)
		return 1
	}
	run, err = prepareRunForExecution(ctx, store, run, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "prepare resumed run: %v\n", err)
		return 1
	}
	providers := inactiveProviders(runtime.LLMTiers)
	var registry *reviewtools.Registry
	var agentLimits reviewtools.AgentLimits
	if run.Status == domain.RunStatusPlanned || run.Status == domain.RunStatusReviewing {
		providers, err = providersForRuntime(runtime)
		if err != nil {
			fmt.Fprintf(stderr, "configure LLM providers: %v\n", err)
			return 1
		}
		toolConfig, configErr := reviewtools.LoadConfig(*toolsPath)
		if configErr != nil {
			fmt.Fprintf(stderr, "load tools config: %v\n", configErr)
			return 1
		}
		snapshot, snapshotErr := store.GetSnapshot(ctx, runID)
		if snapshotErr != nil {
			fmt.Fprintf(stderr, "get run snapshot: %v\n", snapshotErr)
			return 1
		}
		ref, refErr := pullRequestRef(run)
		if refErr != nil {
			fmt.Fprintf(stderr, "restore pull request reference: %v\n", refErr)
			return 1
		}
		apiBaseURL := os.Getenv("GITHUB_API_BASE_URL")
		if apiBaseURL == "" {
			apiBaseURL = defaultGitHubAPI
		}
		adapter, adapterErr := scm.NewGitHubAdapter(nil, apiBaseURL, os.Getenv("GITHUB_TOKEN"))
		if adapterErr != nil {
			fmt.Fprintf(stderr, "create GitHub adapter: %v\n", adapterErr)
			return 1
		}
		registry, err = newToolRegistry(toolConfig, adapter, ref, snapshot)
		if err != nil {
			fmt.Fprintf(stderr, "configure review tools: %v\n", err)
			return 1
		}
		agentLimits = toolConfig.Agent
	}
	owner, err := newExecutorID()
	if err != nil {
		fmt.Fprintf(stderr, "create executor ID: %v\n", err)
		return 1
	}
	engine := newExecutionEngine(store, runtime, owner, providers, runtime.LLMTiers, registry, agentLimits)
	result, err := engine.Execute(ctx, workflow.EngineRequest{
		RunID: runID, Owner: owner, OutputPath: *outputPath,
		Lease: workflow.LeaseSettings{TTL: runtime.LeaseTTL, RenewInterval: runtime.LeaseRenewInterval},
	})
	if err != nil {
		fmt.Fprintf(stderr, "execute resumed run: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run_id=%s\nstatus=%s\ncompleted=%d\nconfirmed=%d\nadvisory=%d\nreport_path=%s\nreused=%t\n", runID, result.Status, result.Review.Completed, result.Aggregation.Confirmed, result.Aggregation.Advisory, result.Report.Report.OutputPath, result.Report.Reused)
	return 0
}

// prepareRunForExecution 补齐 Engine 之前的可恢复阶段：首次抓取和 Review Unit 规划。
// fetched 之后只读取持久化 Snapshot，不会跟随 PR 后续提交。
func prepareRunForExecution(ctx context.Context, store *sqlite.Store, run domain.Run, now time.Time) (domain.Run, error) {
	service := workflow.NewService(store)
	var snapshot domain.ChangeSnapshot
	if run.Status == domain.RunStatusCreated || run.Status == domain.RunStatusFetching {
		ref, err := pullRequestRef(run)
		if err != nil {
			return domain.Run{}, fmt.Errorf("restore pull request reference: %w", err)
		}
		apiBaseURL := os.Getenv("GITHUB_API_BASE_URL")
		if apiBaseURL == "" {
			apiBaseURL = defaultGitHubAPI
		}
		adapter, err := scm.NewGitHubAdapter(nil, apiBaseURL, os.Getenv("GITHUB_TOKEN"))
		if err != nil {
			return domain.Run{}, fmt.Errorf("create GitHub adapter: %w", err)
		}
		if err := service.BeginFetch(ctx, run.ID, now); err != nil {
			return domain.Run{}, err
		}
		snapshot, err = adapter.Fetch(ctx, ref)
		if err != nil {
			return domain.Run{}, fmt.Errorf("fetch pull request: %w", err)
		}
		snapshot = security.NewSanitizer().SanitizeSnapshot(snapshot).Snapshot
		snapshot.CreatedAt = now
		if err := service.CompleteFetch(ctx, run.ID, snapshot, snapshot.CreatedAt); err != nil {
			return domain.Run{}, err
		}
		run.Status = domain.RunStatusFetched
	}
	if run.Status == domain.RunStatusFetched {
		if snapshot.HeadSHA == "" {
			var err error
			snapshot, err = store.GetSnapshot(ctx, run.ID)
			if err != nil {
				return domain.Run{}, fmt.Errorf("get fetched snapshot: %w", err)
			}
		}
		planTime := now
		units := planner.New().Plan(planner.Request{
			RunID: run.ID, HeadSHA: snapshot.HeadSHA, Diff: snapshot.Diff, Now: planTime,
		})
		if err := service.SavePlan(ctx, run.ID, units, planTime); err != nil {
			return domain.Run{}, err
		}
		run.Status = domain.RunStatusPlanned
	}
	return run, nil
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

func newExecutionEngine(store *sqlite.Store, runtime config.Runtime, owner string, providers map[string]llm.Provider, tiers map[string]llm.Tier, registry *reviewtools.Registry, agentLimits reviewtools.AgentLimits) workflow.ExecutionEngine {
	gateway := llm.NewGateway(budget.NewManager(store), llm.ByteUpperBoundCounter{}, providers, tiers)
	reviewer := review.NewReviewer(gateway, runtime.DefaultLLMTier, runtime.MaxFindingsPerUnit)
	if registry != nil {
		reviewer = review.NewRecoverableAgentReviewer(gateway, runtime.DefaultLLMTier, runtime.MaxFindingsPerUnit, registry, agentLimits, store)
	}
	executor := review.NewExecutor(store, reviewer, "llm_review", owner)
	runner := workflow.NewRunner(workflow.NewService(store), executor)
	aggregator := verifier.NewAggregator(store, verifier.NewDefault())
	reporter := report.NewGenerator(store)
	return workflow.NewExecutionEngine(store, runner, aggregator, reporter)
}

func newToolRegistry(config reviewtools.Config, adapter *scm.GitHubAdapter, ref scm.PullRequestRef, snapshot domain.ChangeSnapshot) (*reviewtools.Registry, error) {
	implementations := map[string]reviewtools.Tool{
		"repository_file": reviewtools.NewRepositoryFileTool(adapter, ref, snapshot.HeadSHA),
		"snapshot_search": reviewtools.NewSnapshotSearchTool(snapshot.Diff, 20),
	}
	return reviewtools.NewRegistry(config.Tools, implementations)
}

func pullRequestRef(run domain.Run) (scm.PullRequestRef, error) {
	parts := strings.Split(run.Repository, "/")
	if run.Provider != "github" || len(parts) != 2 || parts[0] == "" || parts[1] == "" || run.ChangeNumber <= 0 {
		return scm.PullRequestRef{}, fmt.Errorf("unsupported persisted SCM identity")
	}
	return scm.PullRequestRef{Owner: parts[0], Repository: parts[1], Number: run.ChangeNumber}, nil
}

func configuredToolsPath() string {
	if value := os.Getenv("REVIEW_AGENT_TOOLS_CONFIG"); value != "" {
		return value
	}
	return defaultToolsPath
}

// providersForRuntime 只从环境变量读取凭据，并按 tier 配置构建 Provider Registry。
func providersForRuntime(runtime config.Runtime) (map[string]llm.Provider, error) {
	providers := make(map[string]llm.Provider)
	for _, tier := range runtime.LLMTiers {
		if _, exists := providers[tier.Provider]; exists {
			continue
		}
		switch tier.Provider {
		case "fake":
			providers["fake"] = &llm.FakeProvider{Response: llm.Response{
				Content: `{"findings":[]}`,
				Usage:   &llm.TokenUsage{},
			}}
		case "openai":
			baseURL := os.Getenv("OPENAI_API_BASE_URL")
			if baseURL == "" {
				baseURL = defaultOpenAIAPI
			}
			provider, err := llm.NewOpenAIProvider(&http.Client{Timeout: runtime.LLMRequestTimeout}, baseURL, os.Getenv("OPENAI_API_KEY"))
			if err != nil {
				return nil, err
			}
			providers["openai"] = provider
		default:
			return nil, fmt.Errorf("unsupported LLM provider %q", tier.Provider)
		}
	}
	return providers, nil
}

// inactiveProviders 只用于 aggregating/reported 阶段；这些阶段不会调用 Provider。
func inactiveProviders(tiers map[string]llm.Tier) map[string]llm.Provider {
	providers := make(map[string]llm.Provider)
	for _, tier := range tiers {
		if _, exists := providers[tier.Provider]; !exists {
			providers[tier.Provider] = &llm.FakeProvider{}
		}
	}
	return providers
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: review-agent <run|status|resume|trace|report> [--db path] [--config path] [id]")
}
