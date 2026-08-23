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
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/llm"
	"github.com/YuHangN/code-review-agent/internal/security"
	"github.com/YuHangN/code-review-agent/internal/tools"
	"github.com/YuHangN/code-review-agent/prompts"
)

var (
	ErrInvalidRequest     = errors.New("invalid review request")
	ErrInvalidResponse    = errors.New("invalid reviewer response")
	ErrAgentLimitExceeded = errors.New("reviewer agent limit exceeded")
)

var hunkHeaderPattern = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// CandidateFinding 是等待 Aggregator 汇总的模型候选问题，不代表最终结论。
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

// KnownDiagnostic 是 Reviewer 去重提示所需的最小 Checker 诊断视图。
type KnownDiagnostic struct {
	Checker string `json:"checker"`
	Code    string `json:"code"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// Request 包含一次 Unit 审查需要的身份和脱敏 diff。
type Request struct {
	CallID           string
	Owner            string
	Unit             domain.ReviewUnit
	Diff             string
	KnownDiagnostics []KnownDiagnostic
}

// Result 保留 Prompt 和原始模型回复，供后续 finding trace 使用。
type Result struct {
	Prompt      string
	RawResponse string
	Findings    []CandidateFinding
	Rejections  []Rejection
	Steps       []AgentStep
}

// AgentStep 保存一轮模型输入、响应和工具 Observation，供 Trace 落盘。
type AgentStep struct {
	Round       int
	CallID      string
	Prompt      string
	Response    string
	ToolCalls   []tools.Call
	ToolResults []ToolResult
}

// ToolResult 同时表达成功结果和被 Registry 拒绝的安全错误。
type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Caller 是 Reviewer 所需的最小模型调用能力。
type Caller interface {
	Call(ctx context.Context, request llm.CallRequest) (llm.Response, error)
}

// AgentCheckpointStore 保存和恢复已经完成的模型轮次。
type AgentCheckpointStore interface {
	ListAgentSteps(ctx context.Context, unitID string) ([]domain.AgentStep, error)
	SaveAgentStep(ctx context.Context, step domain.AgentStep, owner string, now time.Time) error
}

// Reviewer 负责生成 Prompt，并将模型输出解析为候选问题。
type Reviewer struct {
	caller      Caller
	tierOrder   []string
	maxFindings int
	registry    *tools.Registry
	limits      tools.AgentLimits
	checkpoint  AgentCheckpointStore
}

func NewReviewer(caller Caller, tierOrder []string, maxFindings int) Reviewer {
	return Reviewer{caller: caller, tierOrder: append([]string(nil), tierOrder...), maxFindings: maxFindings}
}

// NewAgentReviewer 创建只能调用 Registry 已授权工具的结构化 Reviewer Agent。
func NewAgentReviewer(caller Caller, tierOrder []string, maxFindings int, registry *tools.Registry, limits tools.AgentLimits) Reviewer {
	return Reviewer{caller: caller, tierOrder: append([]string(nil), tierOrder...), maxFindings: maxFindings, registry: registry, limits: limits}
}

// NewRecoverableAgentReviewer 在 Agent Loop 上增加逐轮持久化与恢复。
func NewRecoverableAgentReviewer(caller Caller, tierOrder []string, maxFindings int, registry *tools.Registry, limits tools.AgentLimits, checkpoint AgentCheckpointStore) Reviewer {
	return Reviewer{caller: caller, tierOrder: append([]string(nil), tierOrder...), maxFindings: maxFindings, registry: registry, limits: limits, checkpoint: checkpoint}
}

// Review 只产生 Candidate Finding；Aggregator 会将它们统一标记为 advisory。
func (reviewer Reviewer) Review(ctx context.Context, request Request) (Result, error) {
	if reviewer.caller == nil || len(reviewer.tierOrder) == 0 || reviewer.maxFindings <= 0 || request.CallID == "" || request.Unit.ID == "" || request.Unit.RunID == "" || request.Unit.FilePath == "" || strings.TrimSpace(request.Diff) == "" {
		return Result{}, ErrInvalidRequest
	}
	sanitizedInput := security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: request.Diff})
	prompt, err := reviewer.buildPrompt(request.Unit, sanitizedInput.Snapshot.Diff, request.KnownDiagnostics)
	if err != nil {
		return Result{}, err
	}
	if reviewer.registry != nil {
		return reviewer.reviewWithTools(ctx, request, prompt, sanitizedInput.Snapshot.Diff)
	}
	result := Result{Prompt: prompt}
	response, err := reviewer.caller.Call(ctx, llm.CallRequest{
		ID: request.CallID, RunID: request.Unit.RunID, UnitID: request.Unit.ID, TierOrder: reviewer.tierOrder, Prompt: prompt,
	})
	if err != nil {
		return result, fmt.Errorf("call reviewer model: %w", err)
	}
	sanitizedOutput := security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: response.Content})
	result.RawResponse = sanitizedOutput.Snapshot.Diff
	findings, err := decodeFindings(result.RawResponse)
	if err != nil {
		return result, err
	}
	reviewer.validateFindings(&result, request.Unit, sanitizedInput.Snapshot.Diff, findings)
	return result, nil
}

func (reviewer Reviewer) reviewWithTools(ctx context.Context, request Request, basePrompt, sanitizedDiff string) (Result, error) {
	result := Result{Prompt: basePrompt}
	if reviewer.limits.MaxRounds <= 0 || reviewer.limits.MaxToolCalls <= 0 || (reviewer.checkpoint != nil && request.Owner == "") {
		return result, ErrInvalidRequest
	}
	prompt := basePrompt
	totalToolCalls := 0
	startRound := 1
	if reviewer.checkpoint != nil {
		persisted, err := reviewer.checkpoint.ListAgentSteps(ctx, request.Unit.ID)
		if err != nil {
			return result, fmt.Errorf("load agent checkpoint: %w", err)
		}
		for index, step := range persisted {
			if step.RunID != request.Unit.RunID || step.UnitID != request.Unit.ID || step.Round != index+1 {
				return result, fmt.Errorf("%w: non-contiguous agent checkpoint", ErrInvalidResponse)
			}
			restored := restoreAgentStep(step)
			result.Steps = append(result.Steps, restored)
			result.RawResponse = restored.Response
			toolCalls, findings, mode, decodeErr := decodeAgentResponse(restored.Response)
			if decodeErr != nil {
				return result, decodeErr
			}
			if mode == "findings" {
				reviewer.validateFindings(&result, request.Unit, sanitizedDiff, findings)
				return result, nil
			}
			if len(restored.ToolResults) != len(toolCalls) {
				return result, fmt.Errorf("%w: incomplete tool checkpoint", ErrInvalidResponse)
			}
			totalToolCalls += len(toolCalls)
			if totalToolCalls > reviewer.limits.MaxToolCalls {
				return result, ErrAgentLimitExceeded
			}
			prompt, err = appendAgentTranscript(basePrompt, result.Steps)
			if err != nil {
				return result, err
			}
		}
		startRound = len(persisted) + 1
	}
	if startRound > reviewer.limits.MaxRounds {
		return result, ErrAgentLimitExceeded
	}
	for round := startRound; round <= reviewer.limits.MaxRounds; round++ {
		callID := fmt.Sprintf("%s-round-%d", request.CallID, round)
		response, err := reviewer.caller.Call(ctx, llm.CallRequest{
			ID: callID, RunID: request.Unit.RunID, UnitID: request.Unit.ID, TierOrder: reviewer.tierOrder, Prompt: prompt,
		})
		step := AgentStep{Round: round, CallID: callID, Prompt: prompt}
		if err != nil {
			result.Steps = append(result.Steps, step)
			return result, fmt.Errorf("call reviewer model: %w", err)
		}
		sanitizedOutput := security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: response.Content})
		step.Response = sanitizedOutput.Snapshot.Diff
		result.RawResponse = step.Response
		toolCalls, findings, mode, err := decodeAgentResponse(step.Response)
		if err != nil {
			result.Steps = append(result.Steps, step)
			return result, err
		}
		if mode == "findings" {
			result.Steps = append(result.Steps, step)
			if err := reviewer.saveAgentStep(ctx, request, step); err != nil {
				return result, err
			}
			reviewer.validateFindings(&result, request.Unit, sanitizedDiff, findings)
			return result, nil
		}
		step.ToolCalls = append([]tools.Call(nil), toolCalls...)
		if round == reviewer.limits.MaxRounds || totalToolCalls+len(toolCalls) > reviewer.limits.MaxToolCalls {
			result.Steps = append(result.Steps, step)
			return result, ErrAgentLimitExceeded
		}
		totalToolCalls += len(toolCalls)
		for _, toolCall := range toolCalls {
			observation := ToolResult{CallID: toolCall.ID, Name: toolCall.Name}
			toolResult, executeErr := reviewer.registry.Execute(ctx, toolCall)
			if executeErr != nil {
				observation.Error = sanitizeText(executeErr.Error())
			} else {
				observation.Content = sanitizeText(toolResult.Content)
			}
			step.ToolResults = append(step.ToolResults, observation)
		}
		result.Steps = append(result.Steps, step)
		if err := reviewer.saveAgentStep(ctx, request, step); err != nil {
			return result, err
		}
		prompt, err = appendAgentTranscript(basePrompt, result.Steps)
		if err != nil {
			return result, err
		}
	}
	return result, ErrAgentLimitExceeded
}

func (reviewer Reviewer) saveAgentStep(ctx context.Context, request Request, step AgentStep) error {
	if reviewer.checkpoint == nil {
		return nil
	}
	persisted := domain.AgentStep{
		RunID: request.Unit.RunID, UnitID: request.Unit.ID, Round: step.Round,
		ModelCallID: step.CallID, Prompt: sanitizeText(step.Prompt), Response: sanitizeText(step.Response),
		CreatedAt: time.Now().UTC(),
	}
	for _, call := range step.ToolCalls {
		persisted.ToolCalls = append(persisted.ToolCalls, domain.AgentToolCall{
			ID: sanitizeText(call.ID), Name: sanitizeText(call.Name), Arguments: sanitizeText(string(call.Arguments)),
		})
	}
	for _, toolResult := range step.ToolResults {
		persisted.ToolResults = append(persisted.ToolResults, domain.AgentToolResult{
			CallID: sanitizeText(toolResult.CallID), Name: sanitizeText(toolResult.Name),
			Content: sanitizeText(toolResult.Content), Error: sanitizeText(toolResult.Error),
		})
	}
	if err := reviewer.checkpoint.SaveAgentStep(ctx, persisted, request.Owner, persisted.CreatedAt); err != nil {
		return fmt.Errorf("save agent checkpoint: %w", err)
	}
	return nil
}

func restoreAgentStep(step domain.AgentStep) AgentStep {
	restored := AgentStep{Round: step.Round, CallID: step.ModelCallID, Prompt: step.Prompt, Response: step.Response}
	for _, call := range step.ToolCalls {
		restored.ToolCalls = append(restored.ToolCalls, tools.Call{ID: call.ID, Name: call.Name, Arguments: json.RawMessage(call.Arguments)})
	}
	for _, result := range step.ToolResults {
		restored.ToolResults = append(restored.ToolResults, ToolResult{CallID: result.CallID, Name: result.Name, Content: result.Content, Error: result.Error})
	}
	return restored
}

func (reviewer Reviewer) validateFindings(result *Result, unit domain.ReviewUnit, diff string, findings []CandidateFinding) {
	addedLines := addedLineNumbers(diff)
	for index, finding := range findings {
		switch {
		case index >= reviewer.maxFindings:
			result.Rejections = append(result.Rejections, Rejection{Index: index, Reason: "超过单个 Unit 的 finding 数量上限"})
		case finding.File != unit.FilePath:
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
}

func (reviewer Reviewer) buildPrompt(unit domain.ReviewUnit, diff string, diagnostics []KnownDiagnostic) (string, error) {
	knownDiagnostics, err := encodeKnownDiagnostics(diagnostics)
	if err != nil {
		return "", err
	}
	if reviewer.registry == nil {
		return buildPrompt(unit, diff, knownDiagnostics, reviewer.maxFindings)
	}
	definitionsJSON, err := json.Marshal(reviewer.registry.Definitions())
	if err != nil {
		return "", fmt.Errorf("encode tool definitions: %w", err)
	}
	prompt, err := prompts.RenderAgentReview(prompts.AgentReviewData{
		ReviewData:      prompts.ReviewData{MaxFindings: reviewer.maxFindings, FilePath: unit.FilePath, Risk: unit.Risk, Diff: diff, KnownDiagnostics: knownDiagnostics},
		ToolDefinitions: string(definitionsJSON),
	})
	if err != nil {
		return "", fmt.Errorf("render agent reviewer prompt: %w", err)
	}
	return prompt, nil
}

func decodeFindings(response string) ([]CandidateFinding, error) {
	var payload struct {
		Findings []CandidateFinding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(response), &payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return payload.Findings, nil
}

func decodeAgentResponse(response string) ([]tools.Call, []CandidateFinding, string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(response), &envelope); err != nil {
		return nil, nil, "", fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	toolPayload, hasTools := envelope["tool_calls"]
	findingPayload, hasFindings := envelope["findings"]
	if hasTools == hasFindings || len(envelope) != 1 {
		return nil, nil, "", fmt.Errorf("%w: response must contain exactly one of tool_calls or findings", ErrInvalidResponse)
	}
	if hasTools {
		var calls []tools.Call
		if err := json.Unmarshal(toolPayload, &calls); err != nil || len(calls) == 0 {
			return nil, nil, "", fmt.Errorf("%w: invalid tool_calls", ErrInvalidResponse)
		}
		return calls, nil, "tools", nil
	}
	var findings []CandidateFinding
	if err := json.Unmarshal(findingPayload, &findings); err != nil {
		return nil, nil, "", fmt.Errorf("%w: invalid findings", ErrInvalidResponse)
	}
	return nil, findings, "findings", nil
}

func appendAgentTranscript(basePrompt string, steps []AgentStep) (string, error) {
	type transcriptStep struct {
		Response    string       `json:"assistant_response"`
		ToolResults []ToolResult `json:"tool_results"`
	}
	transcript := make([]transcriptStep, 0, len(steps))
	for _, step := range steps {
		transcript = append(transcript, transcriptStep{Response: step.Response, ToolResults: step.ToolResults})
	}
	encoded, err := json.Marshal(transcript)
	if err != nil {
		return "", fmt.Errorf("encode agent transcript: %w", err)
	}
	return basePrompt + "\n\n<agent_transcript>\n" + string(encoded) + "\n</agent_transcript>\n请根据以上工具结果继续；只输出下一轮 JSON。", nil
}

func sanitizeText(value string) string {
	return security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: value}).Snapshot.Diff
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

func buildPrompt(unit domain.ReviewUnit, diff, knownDiagnostics string, maxFindings int) (string, error) {
	prompt, err := prompts.RenderReview(prompts.ReviewData{
		MaxFindings:      maxFindings,
		FilePath:         unit.FilePath,
		Risk:             unit.Risk,
		Diff:             diff,
		KnownDiagnostics: knownDiagnostics,
	})
	if err != nil {
		return "", fmt.Errorf("render reviewer prompt: %w", err)
	}
	return prompt, nil
}

func encodeKnownDiagnostics(diagnostics []KnownDiagnostic) (string, error) {
	if len(diagnostics) == 0 {
		return "", nil
	}
	sanitized := make([]KnownDiagnostic, len(diagnostics))
	for index, diagnostic := range diagnostics {
		sanitized[index] = KnownDiagnostic{
			Checker: sanitizeText(diagnostic.Checker), Code: sanitizeText(diagnostic.Code),
			File: sanitizeText(diagnostic.File), Line: diagnostic.Line, Message: sanitizeText(diagnostic.Message),
		}
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return "", fmt.Errorf("encode known checker diagnostics: %w", err)
	}
	return string(encoded), nil
}
