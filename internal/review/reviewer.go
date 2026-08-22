// Package review 将 Review Unit 转换为模型可处理的 Prompt 和候选问题。
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/llm"
	"github.com/YuHangN/code-review-agent/internal/security"
	"github.com/YuHangN/code-review-agent/prompts"
)

var (
	ErrInvalidRequest  = errors.New("invalid review request")
	ErrInvalidResponse = errors.New("invalid reviewer response")
)

var hunkHeaderPattern = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// CandidateFinding 是等待 Verifier 复核的候选问题，不代表最终结论。
type CandidateFinding struct {
	Category    string   `json:"category"`
	Severity    string   `json:"severity"`
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Title       string   `json:"title"`
	Explanation string   `json:"explanation"`
	Evidence    []string `json:"evidence"`
	Suggestion  string   `json:"suggestion"`
}

// Rejection 记录被 Reviewer 边界校验拒绝的模型候选项。
type Rejection struct {
	Index  int
	Reason string
}

// Request 包含一次 Unit 审查需要的身份和脱敏 diff。
type Request struct {
	CallID string
	Unit   domain.ReviewUnit
	Diff   string
	Tier   string
}

// Result 保留 Prompt 和原始模型回复，供后续 finding trace 使用。
type Result struct {
	Prompt      string
	RawResponse string
	Findings    []CandidateFinding
	Rejections  []Rejection
}

// Caller 是 Reviewer 所需的最小模型调用能力。
type Caller interface {
	Call(ctx context.Context, request llm.CallRequest) (llm.Response, error)
}

// Reviewer 负责生成 Prompt，并将模型输出解析为候选问题。
type Reviewer struct {
	caller      Caller
	defaultTier string
	maxFindings int
}

func NewReviewer(caller Caller, defaultTier string, maxFindings int) Reviewer {
	return Reviewer{caller: caller, defaultTier: defaultTier, maxFindings: maxFindings}
}

// Review 只产生 Candidate Finding；证据确认和置信度分类由 Verifier 完成。
func (reviewer Reviewer) Review(ctx context.Context, request Request) (Result, error) {
	if reviewer.caller == nil || reviewer.defaultTier == "" || reviewer.maxFindings <= 0 || request.CallID == "" || request.Unit.ID == "" || request.Unit.RunID == "" || request.Unit.FilePath == "" || strings.TrimSpace(request.Diff) == "" {
		return Result{}, ErrInvalidRequest
	}
	tier := request.Tier
	if tier == "" {
		tier = reviewer.defaultTier
	}
	sanitizedInput := security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: request.Diff})
	prompt, err := buildPrompt(request.Unit, sanitizedInput.Snapshot.Diff, reviewer.maxFindings)
	if err != nil {
		return Result{}, err
	}
	result := Result{Prompt: prompt}
	response, err := reviewer.caller.Call(ctx, llm.CallRequest{
		ID: request.CallID, RunID: request.Unit.RunID, UnitID: request.Unit.ID, Tier: tier, Prompt: prompt,
	})
	if err != nil {
		return result, fmt.Errorf("call reviewer model: %w", err)
	}
	sanitizedOutput := security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: response.Content})
	result.RawResponse = sanitizedOutput.Snapshot.Diff
	var payload struct {
		Findings []CandidateFinding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(result.RawResponse), &payload); err != nil {
		return result, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	addedLines := addedLineNumbers(sanitizedInput.Snapshot.Diff)
	for index, finding := range payload.Findings {
		switch {
		case index >= reviewer.maxFindings:
			result.Rejections = append(result.Rejections, Rejection{Index: index, Reason: "超过单个 Unit 的 finding 数量上限"})
		case finding.File != request.Unit.FilePath:
			result.Rejections = append(result.Rejections, Rejection{Index: index, Reason: "file 不属于当前 Review Unit"})
		case !containsLine(addedLines, finding.Line):
			result.Rejections = append(result.Rejections, Rejection{Index: index, Reason: "line 不是当前 diff 的新增行"})
		case !supportedSeverity(finding.Severity):
			result.Rejections = append(result.Rejections, Rejection{Index: index, Reason: "severity 不是允许的等级"})
		case !hasRequiredFields(finding):
			result.Rejections = append(result.Rejections, Rejection{Index: index, Reason: "finding 缺少必填字段或证据"})
		default:
			result.Findings = append(result.Findings, finding)
		}
	}
	return result, nil
}

// addedLineNumbers 按 unified diff 规则计算新文件侧真正新增的行号。
func addedLineNumbers(diff string) map[int]struct{} {
	result := make(map[int]struct{})
	currentLine := 0
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		if match := hunkHeaderPattern.FindStringSubmatch(line); match != nil {
			currentLine, _ = strconv.Atoi(match[1])
			inHunk = true
			continue
		}
		if !inHunk || line == "" || strings.HasPrefix(line, `\ No newline`) {
			continue
		}
		switch line[0] {
		case '+':
			result[currentLine] = struct{}{}
			currentLine++
		case '-':
			// 删除行不占用新文件侧行号。
		default:
			currentLine++
		}
	}
	return result
}

func containsLine(lines map[int]struct{}, line int) bool {
	_, ok := lines[line]
	return ok
}

func supportedSeverity(severity string) bool {
	switch severity {
	case "critical", "high", "medium", "low":
		return true
	default:
		return false
	}
}

func hasRequiredFields(finding CandidateFinding) bool {
	if strings.TrimSpace(finding.Category) == "" || strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Explanation) == "" || strings.TrimSpace(finding.Suggestion) == "" || len(finding.Evidence) == 0 {
		return false
	}
	for _, evidence := range finding.Evidence {
		if strings.TrimSpace(evidence) != "" {
			return true
		}
	}
	return false
}

func buildPrompt(unit domain.ReviewUnit, diff string, maxFindings int) (string, error) {
	prompt, err := prompts.RenderReview(prompts.ReviewData{
		MaxFindings: maxFindings,
		FilePath:    unit.FilePath,
		Risk:        unit.Risk,
		Diff:        diff,
	})
	if err != nil {
		return "", fmt.Errorf("render reviewer prompt: %w", err)
	}
	return prompt, nil
}
