package report

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/security"
)

var (
	ErrInvalidGenerateRequest = errors.New("invalid report generate request")
	ErrReportIntegrity        = errors.New("persisted report content hash mismatch")
)

// Store 是生成和恢复报告所需的最小持久化能力。
type Store interface {
	GetRun(ctx context.Context, runID string) (domain.Run, error)
	GetSnapshot(ctx context.Context, runID string) (domain.ChangeSnapshot, error)
	ListUnits(ctx context.Context, runID string) ([]domain.ReviewUnit, error)
	ListFindings(ctx context.Context, runID string) ([]domain.Finding, error)
	BudgetSummary(ctx context.Context, runID string) (budget.Summary, error)
	SaveReport(ctx context.Context, report domain.Report, now time.Time) error
	GetReport(ctx context.Context, runID string) (domain.Report, error)
}

type checkerStore interface {
	ListCheckerRuns(context.Context, string) ([]domain.CheckerRun, error)
}

type Generator struct {
	store Store
}

type GenerateRequest struct {
	RunID      string
	OutputPath string
	Now        time.Time
}

type GenerateResult struct {
	Report domain.Report
	Reused bool
}

func NewGenerator(store Store) Generator {
	return Generator{store: store}
}

// Generate 原子写入 Markdown。已 reported 的 Run 直接复用 SQLite 中的权威内容。
func (generator Generator) Generate(ctx context.Context, request GenerateRequest) (GenerateResult, error) {
	if generator.store == nil || request.RunID == "" || request.OutputPath == "" || request.Now.IsZero() {
		return GenerateResult{}, ErrInvalidGenerateRequest
	}
	outputPath := filepath.Clean(request.OutputPath)
	run, err := generator.store.GetRun(ctx, request.RunID)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("get report run: %w", err)
	}
	if run.Status == domain.RunStatusReported {
		return generator.restore(ctx, run.ID, outputPath, request.Now)
	}
	if run.Status != domain.RunStatusAggregating {
		return GenerateResult{}, fmt.Errorf("%w: run status is %s", ErrInvalidGenerateRequest, run.Status)
	}

	snapshot, err := generator.store.GetSnapshot(ctx, run.ID)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("get report snapshot: %w", err)
	}
	units, err := generator.store.ListUnits(ctx, run.ID)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("list report units: %w", err)
	}
	findings, err := generator.store.ListFindings(ctx, run.ID)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("list report findings: %w", err)
	}
	budgetSummary, err := generator.store.BudgetSummary(ctx, run.ID)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("summarize report budget: %w", err)
	}
	var checkers []domain.CheckerRun
	if store, ok := generator.store.(checkerStore); ok {
		checkers, err = store.ListCheckerRuns(ctx, run.ID)
		if err != nil {
			return GenerateResult{}, fmt.Errorf("list report checkers: %w", err)
		}
	}
	content, err := Render(Input{Run: run, Snapshot: snapshot, Units: units, Checkers: checkers, Findings: findings, Budget: budgetSummary})
	if err != nil {
		return GenerateResult{}, err
	}
	// 报告落盘前再次扫描，防止未来新增数据源绕过前置脱敏边界。
	content = security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: content}).Snapshot.Diff
	report := domain.Report{
		RunID: run.ID, OutputPath: outputPath, Content: content,
		ContentSHA256: contentHash(content), CreatedAt: request.Now,
	}
	if err := atomicWrite(outputPath, content); err != nil {
		return GenerateResult{}, err
	}
	if err := generator.store.SaveReport(ctx, report, request.Now); err != nil {
		return GenerateResult{}, fmt.Errorf("checkpoint report: %w", err)
	}
	return GenerateResult{Report: report}, nil
}

func (generator Generator) restore(ctx context.Context, runID, outputPath string, now time.Time) (GenerateResult, error) {
	report, err := generator.store.GetReport(ctx, runID)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("get persisted report: %w", err)
	}
	if contentHash(report.Content) != report.ContentSHA256 {
		return GenerateResult{}, ErrReportIntegrity
	}
	if err := atomicWrite(outputPath, report.Content); err != nil {
		return GenerateResult{}, err
	}
	report.OutputPath = outputPath
	if err := generator.store.SaveReport(ctx, report, now); err != nil {
		return GenerateResult{}, fmt.Errorf("checkpoint restored report: %w", err)
	}
	return GenerateResult{Report: report, Reused: true}, nil
}

func atomicWrite(path, content string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".review-report-*")
	if err != nil {
		return fmt.Errorf("create temporary report: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set report permissions: %w", err)
	}
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary report: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary report: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace report: %w", err)
	}
	return nil
}

func contentHash(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", hash[:])
}
