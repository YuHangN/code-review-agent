// Package app 负责组装组件，并执行用户可见的 run 和 resume 用例。
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	defaultGitHubAPI = "https://api.github.com"
	defaultOpenAIAPI = "https://api.openai.com"
	microsPerCent    = int64(10_000)
)

var ErrInvalidRequest = errors.New("invalid application request")

// Application 是 run/resume 的应用入口。CLI 只需解析参数并调用它。
type Application struct {
	store     *sqlite.Store
	runtime   config.Runtime
	toolsPath string
}

// StartRequest 描述一次全新的 PR 审查。
type StartRequest struct {
	URL          string
	PullRequest  scm.PullRequestRef
	BudgetCents  int64
	OutputPath   string
	OnRunCreated func(runID string)
}

// StartResult 包含首次抓取信息和最终执行结果。
type StartResult struct {
	RunID         string
	BaseSHA       string
	HeadSHA       string
	Units         int
	Redactions    int
	ExcludedFiles int
	Execution     workflow.Result
}

// ResumeRequest 描述从 checkpoint 继续执行所需的信息。
type ResumeRequest struct {
	RunID      string
	OutputPath string
}

// New 创建应用入口；具体 Reviewer、Tool 和 Workflow 会按 Run 状态延迟组装。
func New(store *sqlite.Store, runtime config.Runtime, toolsPath string) Application {
	return Application{store: store, runtime: runtime, toolsPath: toolsPath}
}

// ValidBudgetCents 判断美元分预算能否安全转换为内部微美元。
func ValidBudgetCents(value int64) bool {
	return value > 0 && value <= math.MaxInt64/microsPerCent
}

// Start 创建 Run、固定 Snapshot、生成计划，并执行到 Markdown 报告。
func (application Application) Start(ctx context.Context, request StartRequest) (StartResult, error) {
	if application.store == nil || request.URL == "" || request.PullRequest.Owner == "" || !ValidBudgetCents(request.BudgetCents) {
		return StartResult{}, ErrInvalidRequest
	}
	providers, err := application.providers()
	if err != nil {
		return StartResult{}, fmt.Errorf("configure LLM providers: %w", err)
	}
	toolConfig, err := reviewtools.LoadConfig(application.toolsPath)
	if err != nil {
		return StartResult{}, fmt.Errorf("load tools config: %w", err)
	}
	adapter, err := application.githubAdapter()
	if err != nil {
		return StartResult{}, fmt.Errorf("create GitHub adapter: %w", err)
	}

	runID, err := newID("run")
	if err != nil {
		return StartResult{}, fmt.Errorf("create run ID: %w", err)
	}
	result := StartResult{RunID: runID}
	if request.OutputPath == "" {
		request.OutputPath = filepath.Join("out", runID+".md")
	}
	now := time.Now().UTC()
	run := domain.Run{
		ID:                runID,
		SourceURL:         request.URL,
		Provider:          "github",
		Repository:        request.PullRequest.Owner + "/" + request.PullRequest.Repository,
		ChangeNumber:      request.PullRequest.Number,
		Status:            domain.RunStatusCreated,
		BudgetLimitMicros: request.BudgetCents * microsPerCent,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := application.store.CreateRun(ctx, run, nil); err != nil {
		return result, fmt.Errorf("create run: %w", err)
	}
	if request.OnRunCreated != nil {
		request.OnRunCreated(runID)
	}

	snapshot, sanitized, err := application.fetchSnapshot(ctx, run, request.PullRequest, adapter)
	if err != nil {
		return result, err
	}
	units := planner.New().Plan(planner.Request{
		RunID: runID, HeadSHA: snapshot.HeadSHA, Diff: snapshot.Diff, Now: time.Now().UTC(),
	})
	if err := application.store.SavePlan(ctx, runID, units, time.Now().UTC()); err != nil {
		return result, fmt.Errorf("save review plan: %w", err)
	}
	result.BaseSHA = snapshot.BaseSHA
	result.HeadSHA = snapshot.HeadSHA
	result.Units = len(units)
	result.Redactions = len(sanitized.Redactions)
	result.ExcludedFiles = len(sanitized.ExcludedFiles)

	owner, err := newWorkerID()
	if err != nil {
		return result, fmt.Errorf("create worker ID: %w", err)
	}
	registry, err := newToolRegistry(toolConfig, adapter, request.PullRequest, snapshot)
	if err != nil {
		return result, fmt.Errorf("configure review tools: %w", err)
	}
	result.Execution, err = application.execute(ctx, runID, owner, request.OutputPath, providers, registry, toolConfig.Agent)
	if err != nil {
		return result, fmt.Errorf("execute review: %w", err)
	}
	return result, nil
}

// Resume 从数据库状态继续；已完成的抓取、规划和 Unit 不会重做。
func (application Application) Resume(ctx context.Context, request ResumeRequest) (workflow.Result, error) {
	if application.store == nil || request.RunID == "" {
		return workflow.Result{}, ErrInvalidRequest
	}
	if request.OutputPath == "" {
		request.OutputPath = filepath.Join("out", request.RunID+".md")
	}
	run, err := application.store.GetRun(ctx, request.RunID)
	if err != nil {
		return workflow.Result{}, fmt.Errorf("get run: %w", err)
	}
	run, err = application.prepare(ctx, run)
	if err != nil {
		return workflow.Result{}, fmt.Errorf("prepare resumed run: %w", err)
	}

	providers := inactiveProviders(application.runtime.LLMTiers)
	var registry *reviewtools.Registry
	var agentLimits reviewtools.AgentLimits
	if run.Status == domain.RunStatusPlanned || run.Status == domain.RunStatusReviewing {
		providers, err = application.providers()
		if err != nil {
			return workflow.Result{}, fmt.Errorf("configure LLM providers: %w", err)
		}
		toolConfig, loadErr := reviewtools.LoadConfig(application.toolsPath)
		if loadErr != nil {
			return workflow.Result{}, fmt.Errorf("load tools config: %w", loadErr)
		}
		snapshot, snapshotErr := application.store.GetSnapshot(ctx, run.ID)
		if snapshotErr != nil {
			return workflow.Result{}, fmt.Errorf("get run snapshot: %w", snapshotErr)
		}
		ref, refErr := pullRequestRef(run)
		if refErr != nil {
			return workflow.Result{}, fmt.Errorf("restore pull request reference: %w", refErr)
		}
		adapter, adapterErr := application.githubAdapter()
		if adapterErr != nil {
			return workflow.Result{}, fmt.Errorf("create GitHub adapter: %w", adapterErr)
		}
		registry, err = newToolRegistry(toolConfig, adapter, ref, snapshot)
		if err != nil {
			return workflow.Result{}, fmt.Errorf("configure review tools: %w", err)
		}
		agentLimits = toolConfig.Agent
	}
	owner, err := newWorkerID()
	if err != nil {
		return workflow.Result{}, fmt.Errorf("create worker ID: %w", err)
	}
	result, err := application.execute(ctx, run.ID, owner, request.OutputPath, providers, registry, agentLimits)
	if err != nil {
		return result, fmt.Errorf("execute resumed run: %w", err)
	}
	return result, nil
}

// prepare 只补齐尚未完成的 fetch 和 plan checkpoint。
func (application Application) prepare(ctx context.Context, run domain.Run) (domain.Run, error) {
	var snapshot domain.ChangeSnapshot
	if run.Status == domain.RunStatusCreated || run.Status == domain.RunStatusFetching {
		ref, err := pullRequestRef(run)
		if err != nil {
			return domain.Run{}, fmt.Errorf("restore pull request reference: %w", err)
		}
		adapter, adapterErr := application.githubAdapter()
		if adapterErr != nil {
			return domain.Run{}, fmt.Errorf("create GitHub adapter: %w", adapterErr)
		}
		snapshot, _, err = application.fetchSnapshot(ctx, run, ref, adapter)
		if err != nil {
			return domain.Run{}, err
		}
		run.Status = domain.RunStatusFetched
	}
	if run.Status == domain.RunStatusFetched {
		if snapshot.HeadSHA == "" {
			var err error
			snapshot, err = application.store.GetSnapshot(ctx, run.ID)
			if err != nil {
				return domain.Run{}, fmt.Errorf("get fetched snapshot: %w", err)
			}
		}
		now := time.Now().UTC()
		units := planner.New().Plan(planner.Request{RunID: run.ID, HeadSHA: snapshot.HeadSHA, Diff: snapshot.Diff, Now: now})
		if err := application.store.SavePlan(ctx, run.ID, units, now); err != nil {
			return domain.Run{}, fmt.Errorf("save review plan: %w", err)
		}
		run.Status = domain.RunStatusPlanned
	}
	return run, nil
}

func (application Application) fetchSnapshot(ctx context.Context, run domain.Run, ref scm.PullRequestRef, adapter *scm.GitHubAdapter) (domain.ChangeSnapshot, security.Result, error) {
	if err := application.store.BeginFetch(ctx, run.ID, time.Now().UTC()); err != nil {
		return domain.ChangeSnapshot{}, security.Result{}, fmt.Errorf("begin fetch: %w", err)
	}
	snapshot, err := adapter.Fetch(ctx, ref)
	if err != nil {
		return domain.ChangeSnapshot{}, security.Result{}, fmt.Errorf("fetch pull request: %w", err)
	}
	sanitized := security.NewSanitizer().SanitizeSnapshot(snapshot)
	snapshot = sanitized.Snapshot
	snapshot.CreatedAt = time.Now().UTC()
	if err := application.store.SaveFetchedSnapshot(ctx, run.ID, snapshot, snapshot.CreatedAt); err != nil {
		return domain.ChangeSnapshot{}, security.Result{}, fmt.Errorf("save fetched snapshot: %w", err)
	}
	return snapshot, sanitized, nil
}

func (application Application) execute(ctx context.Context, runID, owner, outputPath string, providers map[string]llm.Provider, registry *reviewtools.Registry, agentLimits reviewtools.AgentLimits) (workflow.Result, error) {
	gateway := llm.NewGateway(budget.NewManager(application.store), llm.ByteUpperBoundCounter{}, providers, application.runtime.LLMTiers)
	reviewer := review.NewReviewer(gateway, application.runtime.DefaultLLMTier, application.runtime.MaxFindingsPerUnit)
	if registry != nil {
		reviewer = review.NewRecoverableAgentReviewer(gateway, application.runtime.DefaultLLMTier, application.runtime.MaxFindingsPerUnit, registry, agentLimits, application.store)
	}
	processor := review.NewUnitProcessor(application.store, reviewer, "llm_review", owner)
	flow := workflow.New(application.store, processor, verifier.NewAggregator(application.store, verifier.NewDefault()), report.NewGenerator(application.store))
	return flow.Execute(ctx, workflow.ExecuteRequest{
		RunID: runID, Owner: owner, OutputPath: outputPath,
		Lease: workflow.LeaseSettings{TTL: application.runtime.LeaseTTL, RenewInterval: application.runtime.LeaseRenewInterval},
	})
}

func (application Application) githubAdapter() (*scm.GitHubAdapter, error) {
	baseURL := os.Getenv("GITHUB_API_BASE_URL")
	if baseURL == "" {
		baseURL = defaultGitHubAPI
	}
	return scm.NewGitHubAdapter(nil, baseURL, os.Getenv("GITHUB_TOKEN"))
}

func (application Application) providers() (map[string]llm.Provider, error) {
	providers := make(map[string]llm.Provider)
	for _, tier := range application.runtime.LLMTiers {
		if _, exists := providers[tier.Provider]; exists {
			continue
		}
		switch tier.Provider {
		case "fake":
			providers["fake"] = &llm.FakeProvider{Response: llm.Response{Content: `{"findings":[]}`, Usage: &llm.TokenUsage{}}}
		case "openai":
			baseURL := os.Getenv("OPENAI_API_BASE_URL")
			if baseURL == "" {
				baseURL = defaultOpenAIAPI
			}
			provider, err := llm.NewOpenAIProvider(&http.Client{Timeout: application.runtime.LLMRequestTimeout}, baseURL, os.Getenv("OPENAI_API_KEY"))
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

func inactiveProviders(tiers map[string]llm.Tier) map[string]llm.Provider {
	providers := make(map[string]llm.Provider)
	for _, tier := range tiers {
		if _, exists := providers[tier.Provider]; !exists {
			providers[tier.Provider] = &llm.FakeProvider{}
		}
	}
	return providers
}

// newWorkerID 组合主机、PID 和随机值，便于排查当前 lease 的持有者。
func newWorkerID() (string, error) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	id, err := newID("")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("cli-%s-%d-%s", hostname, os.Getpid(), id), nil
}

func newID(prefix string) (string, error) {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	id := hex.EncodeToString(bytes[:])
	if prefix != "" {
		return prefix + "-" + id, nil
	}
	return id, nil
}
