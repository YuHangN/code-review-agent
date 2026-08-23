package report_test

import (
	"strings"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/report"
)

func TestRenderProducesDeterministicReviewReport(t *testing.T) {
	input := report.Input{
		Run:      domain.Run{ID: "run-001", Repository: "acme/payments", ChangeNumber: 42, BudgetLimitMicros: 10_000_000},
		Snapshot: domain.ChangeSnapshot{BaseSHA: "base-123", HeadSHA: "head-456"},
		Units: []domain.ReviewUnit{
			{ID: "unit-a", Status: domain.UnitStatusCompleted},
			{ID: "unit-b", Status: domain.UnitStatusSkippedBudget},
		},
		Checkers: []domain.CheckerRun{{Checker: "go_vet", Status: domain.CheckerStatusCompleted, Attempt: 1}, {Checker: "staticcheck", Status: domain.CheckerStatusCompleted, Attempt: 1}},
		Budget: budget.Summary{
			ActualMicros: 420_000, CommittedMicros: 420_000,
			Tiers: []budget.TierSummary{
				{Name: "economy", SettledCalls: 3, ActualMicros: 120_000},
				{Name: "strong", SettledCalls: 2, ActualMicros: 300_000},
			},
		},
		Findings: []domain.Finding{
			{ID: "finding-advisory", TraceID: "trace-advisory", Confidence: domain.ConfidenceAdvisory, VerificationSource: "llm_reasoning_only", VerificationReason: "只有模型推理", Severity: "medium", File: "cache.go", Line: 8, Title: "共享 map 可能并发访问", Explanation: "可能发生竞态", Evidence: []string{"goroutine 写入 map"}, Suggestion: "检查同步方式"},
			{ID: "finding-confirmed", TraceID: "trace-confirmed", Confidence: domain.ConfidenceConfirmed, VerificationSource: "checker:staticcheck", VerificationReason: "确定性静态检查器命中", Severity: "high", File: "config.yaml", Line: 2, Title: "配置中包含凭据", Explanation: "凭据写入源码", Evidence: []string{"staticcheck 命中"}, Suggestion: "改用密钥服务"},
		},
	}

	first, err := report.Render(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := report.Render(input)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("same input generated different reports")
	}
	for _, want := range []string{
		"# 代码审查报告",
		"acme/payments #42",
		"`base-123` → `head-456`",
		"已审查 Unit：1 / 2",
		"预算跳过 Unit：1",
		"$0.420000 / $10.000000",
		"模型 Tier：economy 3 次（$0.120000），strong 2 次（$0.300000）",
		"Checker：go_vet=completed（1 次），staticcheck=completed（1 次）",
		"## 高置信度，可直接采纳（1）",
		"config.yaml:2",
		"checker:staticcheck",
		"trace-confirmed",
		"## 仅供参考（1）",
		"cache.go:8",
		"llm_reasoning_only",
		"trace-advisory",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("report does not contain %q:\n%s", want, first)
		}
	}
	if strings.Index(first, "配置中包含凭据") > strings.Index(first, "共享 map 可能并发访问") {
		t.Fatal("confirmed section was rendered after advisory section")
	}
}
