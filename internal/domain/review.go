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

// CandidateFindingRecord 是尚未经过 Verifier 的持久化候选问题。
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

// VerifiedFinding 是经过确定性规则验证和去重后可进入报告的最终问题。
type VerifiedFinding struct {
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
