package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/YuHangN/code-review-agent/internal/app"
	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/config"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/report"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

const (
	defaultDBPath     = "review-agent.db"
	defaultConfigPath = "config/runtime.yaml"
	defaultToolsPath  = "config/tools.yaml"
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
		fmt.Fprintln(stderr, "run requires a pull request or merge request URL")
		return 2
	}
	if budgetOverridden && !app.ValidBudgetCents(*budgetCents) {
		fmt.Fprintln(stderr, "budget-cents must be a positive integer within range")
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
	if !app.ValidBudgetCents(effectiveBudgetCents) {
		fmt.Fprintln(stderr, "configured default budget is outside the supported range")
		return 1
	}
	application := app.New(store, runtimeConfig, *toolsPath)
	result, err := application.Start(ctx, app.StartRequest{
		URL:         flags.Args()[0],
		BudgetCents: effectiveBudgetCents,
		OutputPath:  *outputPath,
		OnRunCreated: func(runID string) {
			// Run 持久化后立即输出 ID；后续断网仍可用 resume 继续。
			fmt.Fprintf(stdout, "run_id=%s\n", runID)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "run review: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "base_sha=%s\nhead_sha=%s\nunits=%d\nredactions=%d\nexcluded_files=%d\n", result.BaseSHA, result.HeadSHA, result.Units, result.Redactions, result.ExcludedFiles)
	fmt.Fprintf(stdout, "status=%s\nreport_path=%s\n", result.Execution.Status, result.Execution.Report.Report.OutputPath)
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
	candidates, err := store.ListCandidateFindings(ctx, trace.RunID)
	if err != nil {
		fmt.Fprintf(stderr, "list trace findings: %v\n", err)
		return 1
	}
	findings, err := store.ListFindings(ctx, trace.RunID)
	if err != nil {
		fmt.Fprintf(stderr, "list findings: %v\n", err)
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
	for _, finding := range candidates {
		if finding.TraceID != trace.ID {
			continue
		}
		fmt.Fprintf(stdout, "candidate_id=%s\nfile=%s\nline=%d\nseverity=%s\ntitle=%s\n", finding.ID, finding.File, finding.Line, finding.Severity, finding.Title)
		fmt.Fprintf(stdout, "explanation=%s\nsuggestion=%s\n", finding.Explanation, finding.Suggestion)
		for _, evidence := range finding.Evidence {
			fmt.Fprintf(stdout, "evidence=%s\n", evidence)
		}
	}
	for _, finding := range findings {
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
	result, err := app.New(store, runtime, *toolsPath).Resume(ctx, app.ResumeRequest{RunID: runID, OutputPath: *outputPath})
	if err != nil {
		fmt.Fprintf(stderr, "resume review: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "run_id=%s\nstatus=%s\ncompleted=%d\nconfirmed=%d\nadvisory=%d\nreport_path=%s\nreused=%t\n", runID, result.Status, result.Units.Completed, result.Aggregation.Confirmed, result.Aggregation.Advisory, result.Report.Report.OutputPath, result.Report.Reused)
	return 0
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

func configuredToolsPath() string {
	if value := os.Getenv("REVIEW_AGENT_TOOLS_CONFIG"); value != "" {
		return value
	}
	return defaultToolsPath
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: review-agent <run|status|resume|trace|report> [--db path] [--config path] [id]")
}
