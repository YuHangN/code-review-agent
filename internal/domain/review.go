package domain

import "time"

// TraceRejection 保存 Reviewer 拒绝的模型候选及原因。
type TraceRejection struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// ReviewTrace 是一次 Unit 模型审查的脱敏证据链。
type ReviewTrace struct {
	ID           string
	RunID        string
	UnitID       string
	CallID       string
	Detector     string
	Status       string
	Prompt       string
	Response     string
	Rejections   []TraceRejection
	ErrorMessage string
	CreatedAt    time.Time
}

// AgentToolCall 是一轮中经过脱敏后持久化的模型工具请求。
type AgentToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// AgentToolResult 是返回模型的脱敏 Observation。
type AgentToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// AgentStep 是一个可独立恢复的模型轮次 checkpoint。
type AgentStep struct {
	RunID       string
	UnitID      string
	Round       int
	ModelCallID string
	Prompt      string
	Response    string
	ToolCalls   []AgentToolCall
	ToolResults []AgentToolResult
	CreatedAt   time.Time
}

// CandidateFindingRecord 是尚待 Aggregator 汇总的持久化 LLM 候选问题。
type CandidateFindingRecord struct {
	ID          string
	RunID       string
	UnitID      string
	TraceID     string
	Detector    string
	Category    string
	Severity    string
	File        string
	Line        int
	Title       string
	Explanation string
	Evidence    []string
	Suggestion  string
	CreatedAt   time.Time
}

// Confidence 表示最终 Finding 的证据等级，不接受模型自报概率。
type Confidence string

const (
	ConfidenceConfirmed Confidence = "confirmed"
	ConfidenceAdvisory  Confidence = "advisory"
)

// Finding 是完成来源分级和去重后可进入报告的统一最终问题。
type Finding struct {
	ID                 string
	RunID              string
	CandidateID        string
	TraceID            string
	Fingerprint        string
	Confidence         Confidence
	VerificationSource string
	VerificationReason string
	Category           string
	Severity           string
	File               string
	Line               int
	Title              string
	Explanation        string
	Evidence           []string
	Suggestion         string
	CreatedAt          time.Time
}
