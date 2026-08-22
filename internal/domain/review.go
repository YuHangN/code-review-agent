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
