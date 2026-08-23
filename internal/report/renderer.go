// Package report 将最终 Finding 渲染并保存为确定性的 Markdown 报告。
package report

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
)

var ErrInvalidInput = errors.New("invalid report input")

// Input 包含报告所需的持久化事实，不触发模型或工具调用。
type Input struct {
	Run      domain.Run
	Snapshot domain.ChangeSnapshot
	Units    []domain.ReviewUnit
	Checkers []domain.CheckerRun
	Findings []domain.Finding
	Budget   budget.Summary
}

// Render 将相同输入稳定地渲染为相同 Markdown。
func Render(input Input) (string, error) {
	if input.Run.ID == "" || input.Run.Repository == "" || input.Snapshot.BaseSHA == "" || input.Snapshot.HeadSHA == "" {
		return "", ErrInvalidInput
	}
	confirmed, advisory := splitAndSortFindings(input.Findings)
	completed, skippedBudget := unitCoverage(input.Units)

	var output strings.Builder
	fmt.Fprintln(&output, "# Code Review Report")
	fmt.Fprintln(&output)
	fmt.Fprintf(&output, "**变更：** %s #%d  \n", singleLine(input.Run.Repository), input.Run.ChangeNumber)
	fmt.Fprintf(&output, "**版本：** `%s` → `%s`\n\n", singleLine(input.Snapshot.BaseSHA), singleLine(input.Snapshot.HeadSHA))
	fmt.Fprintln(&output, "## 审查摘要")
	fmt.Fprintln(&output)
	fmt.Fprintf(&output, "- 已审查 Unit：%d / %d\n", completed, len(input.Units))
	fmt.Fprintf(&output, "- 预算跳过 Unit：%d\n", skippedBudget)
	fmt.Fprintf(&output, "- 高置信度：%d\n", len(confirmed))
	fmt.Fprintf(&output, "- 仅供参考：%d\n", len(advisory))
	fmt.Fprintf(&output, "- 预算使用：%s / %s\n", formatUSD(input.Budget.CommittedMicros), formatUSD(input.Run.BudgetLimitMicros))
	if len(input.Budget.Tiers) > 0 {
		fmt.Fprintf(&output, "- 模型 Tier：%s\n", formatTierUsage(input.Budget.Tiers))
	}
	if len(input.Checkers) > 0 {
		fmt.Fprintf(&output, "- Checker：%s\n", formatCheckers(input.Checkers))
	}

	renderFindingSection(&output, "高置信度，可直接采纳", confirmed)
	renderFindingSection(&output, "仅供参考", advisory)
	return output.String(), nil
}

func formatCheckers(checkers []domain.CheckerRun) string {
	items := append([]domain.CheckerRun(nil), checkers...)
	sort.Slice(items, func(i, j int) bool { return items[i].Checker < items[j].Checker })
	formatted := make([]string, 0, len(items))
	for _, checker := range items {
		formatted = append(formatted, fmt.Sprintf("%s=%s（%d 次）", singleLine(checker.Checker), checker.Status, checker.Attempt))
	}
	return strings.Join(formatted, "，")
}

func formatTierUsage(tiers []budget.TierSummary) string {
	items := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		items = append(items, fmt.Sprintf("%s %d 次（%s）", singleLine(tier.Name), tier.SettledCalls, formatUSD(tier.ActualMicros)))
	}
	return strings.Join(items, "，")
}

func splitAndSortFindings(findings []domain.Finding) ([]domain.Finding, []domain.Finding) {
	var confirmed, advisory []domain.Finding
	for _, finding := range findings {
		if finding.Confidence == domain.ConfidenceConfirmed {
			confirmed = append(confirmed, finding)
		} else {
			advisory = append(advisory, finding)
		}
	}
	sortFindings(confirmed)
	sortFindings(advisory)
	return confirmed, advisory
}

func sortFindings(findings []domain.Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].ID < findings[j].ID
	})
}

func unitCoverage(units []domain.ReviewUnit) (completed, skippedBudget int) {
	for _, unit := range units {
		switch unit.Status {
		case domain.UnitStatusCompleted:
			completed++
		case domain.UnitStatusSkippedBudget:
			skippedBudget++
		}
	}
	return completed, skippedBudget
}

func renderFindingSection(output *strings.Builder, title string, findings []domain.Finding) {
	fmt.Fprintf(output, "\n## %s（%d）\n\n", title, len(findings))
	if len(findings) == 0 {
		fmt.Fprintln(output, "无。")
		return
	}
	for _, finding := range findings {
		fmt.Fprintf(output, "### %s\n\n", singleLine(finding.Title))
		fmt.Fprintf(output, "- 位置：`%s:%d`\n", singleLine(finding.File), finding.Line)
		fmt.Fprintf(output, "- 严重度：%s\n", singleLine(finding.Severity))
		fmt.Fprintf(output, "- 验证来源：`%s`\n", singleLine(finding.VerificationSource))
		fmt.Fprintf(output, "- 验证说明：%s\n", singleLine(finding.VerificationReason))
		fmt.Fprintf(output, "- Trace：`%s`\n\n", singleLine(finding.TraceID))
		fmt.Fprintf(output, "问题：%s\n\n", strings.TrimSpace(finding.Explanation))
		fmt.Fprintln(output, "证据：")
		for _, evidence := range finding.Evidence {
			fmt.Fprintf(output, "- %s\n", singleLine(evidence))
		}
		fmt.Fprintf(output, "\n建议：%s\n\n", strings.TrimSpace(finding.Suggestion))
	}
}

func formatUSD(micros int64) string {
	return fmt.Sprintf("$%.6f", float64(micros)/1_000_000)
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
