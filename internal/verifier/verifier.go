// Package verifier 将 Reviewer 产生的候选问题转换为有证据等级的最终 Finding。
package verifier

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

var (
	hunkHeaderPattern = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)
	secretKeyPattern  = regexp.MustCompile(`(?i)(password|token|api[_-]?key|secret)\s*[:=]`)
)

// RuleResult 描述一条确定性规则是否为候选问题提供了可复核证据。
type RuleResult struct {
	Matched  bool
	Source   string
	Reason   string
	Evidence string
}

// EvidenceRule 只复核 LLM Reviewer 的 Candidate，不主动发现或生成 Finding。
type EvidenceRule interface {
	Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit, steps []domain.AgentStep) RuleResult
}

// Verifier 按注册顺序复核 LLM Candidate，首个确定性证据命中即可确认候选问题。
type Verifier struct {
	rules []EvidenceRule
}

// NewDefault 创建首版内置规则集合。
func NewDefault() Verifier {
	return Verifier{rules: []EvidenceRule{toolBackedSecretAssignmentRule{}, redactedSecretAssignmentRule{}}}
}

// Verify 根据确定性证据分类；未命中任何规则的候选只能是 advisory。
func (verifier Verifier) Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit, now time.Time) domain.VerifiedFinding {
	return verifier.VerifyWithEvidence(candidate, unit, nil, now)
}

// VerifyWithEvidence 在 Unit diff 之外读取已持久化的工具 Observation。
// 工具调用本身不会提高置信度，只有能够确定性解析并关联到候选位置的结果才会参与规则判断。
func (verifier Verifier) VerifyWithEvidence(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit, steps []domain.AgentStep, now time.Time) domain.VerifiedFinding {
	fingerprint := findingFingerprint(candidate)
	finding := domain.VerifiedFinding{
		ID: findingID(candidate.RunID, fingerprint), RunID: candidate.RunID,
		CandidateID: candidate.ID, TraceID: candidate.TraceID, Fingerprint: fingerprint,
		Confidence: domain.ConfidenceAdvisory, VerificationSource: "llm_reasoning_only",
		VerificationReason: "未获得确定性规则或工具证据，仅保留为参考建议",
		Category:           candidate.Category, Severity: candidate.Severity, File: candidate.File,
		Line: candidate.Line, Title: candidate.Title, Explanation: candidate.Explanation,
		Evidence: append([]string(nil), candidate.Evidence...), Suggestion: candidate.Suggestion, CreatedAt: now,
	}
	for _, rule := range verifier.rules {
		result := rule.Verify(candidate, unit, steps)
		if !result.Matched {
			continue
		}
		finding.Confidence = domain.ConfidenceConfirmed
		finding.VerificationSource = result.Source
		finding.VerificationReason = result.Reason
		if result.Evidence != "" {
			finding.Evidence = append(finding.Evidence, result.Evidence)
		}
		break
	}
	return finding
}

type redactedSecretAssignmentRule struct{}

// Verify 利用脱敏占位符确认新增行曾包含疑似凭据，同时不接触原始 secret。
func (redactedSecretAssignmentRule) Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit, _ []domain.AgentStep) RuleResult {
	if !strings.EqualFold(candidate.Category, "security") {
		return RuleResult{}
	}
	line, ok := addedLineAt(unit.DiffHunk, candidate.Line)
	if !ok || !strings.Contains(line, "<REDACTED:") || !secretKeyPattern.MatchString(line) {
		return RuleResult{}
	}
	return RuleResult{
		Matched: true, Source: "rule:redacted_secret_assignment",
		Reason:   "新增行中的凭据字段被 Secret Scanner 确定性命中并脱敏",
		Evidence: fmt.Sprintf("新增行 %d 同时包含凭据字段赋值和可追踪脱敏占位符", candidate.Line),
	}
}

type toolBackedSecretAssignmentRule struct{}

type readFileArguments struct {
	Path string `json:"path"`
}

type readFileResult struct {
	Path       string `json:"path"`
	SHA        string `json:"sha"`
	Content    string `json:"content"`
	Redactions int    `json:"redactions"`
}

// Verify 只接受与候选文件、行号和调用 ID 完整关联的成功 read_file 结果。
func (toolBackedSecretAssignmentRule) Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit, steps []domain.AgentStep) RuleResult {
	if !strings.EqualFold(candidate.Category, "security") || candidate.Line <= 0 || !sameRepositoryPath(unit.FilePath, candidate.File) {
		return RuleResult{}
	}
	diffLine, ok := addedLineAt(unit.DiffHunk, candidate.Line)
	if !ok || !strings.Contains(diffLine, "<REDACTED:") || !secretKeyPattern.MatchString(diffLine) {
		return RuleResult{}
	}
	for _, step := range steps {
		for _, call := range step.ToolCalls {
			if call.Name != "read_file" {
				continue
			}
			var arguments readFileArguments
			if json.Unmarshal([]byte(call.Arguments), &arguments) != nil || !sameRepositoryPath(arguments.Path, candidate.File) {
				continue
			}
			for _, observation := range step.ToolResults {
				if observation.CallID != call.ID || observation.Name != call.Name || observation.Error != "" {
					continue
				}
				var result readFileResult
				if json.Unmarshal([]byte(observation.Content), &result) != nil || result.SHA == "" || result.Redactions <= 0 || !sameRepositoryPath(result.Path, candidate.File) {
					continue
				}
				line, ok := fileLineAt(result.Content, candidate.Line)
				if !ok || strings.TrimSpace(line) != strings.TrimSpace(diffLine) {
					continue
				}
				return RuleResult{
					Matched: true, Source: "tool:read_file+rule:redacted_secret_assignment",
					Reason:   "固定 SHA 文件读取结果与候选位置一致，且 Secret Scanner 确定性命中凭据赋值",
					Evidence: fmt.Sprintf("read_file 在 %s:%d 返回凭据字段和可追踪脱敏占位符", candidate.File, candidate.Line),
				}
			}
		}
	}
	return RuleResult{}
}

func sameRepositoryPath(left, right string) bool {
	return left != "" && right != "" && path.Clean(left) == path.Clean(right)
}

func fileLineAt(content string, target int) (string, bool) {
	lines := strings.Split(content, "\n")
	if target <= 0 || target > len(lines) {
		return "", false
	}
	return lines[target-1], true
}

func addedLineAt(diff string, target int) (string, bool) {
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
			if currentLine == target {
				return strings.TrimPrefix(line, "+"), true
			}
			currentLine++
		case '-':
		default:
			currentLine++
		}
	}
	return "", false
}

func findingFingerprint(candidate domain.CandidateFindingRecord) string {
	return findingFingerprintParts(candidate.Category, candidate.File, candidate.Line, candidate.Title)
}

func findingFingerprintParts(category, file string, line int, issueType string) string {
	identity := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(file)),
		strconv.Itoa(line),
		strings.ToLower(strings.TrimSpace(category)),
		normalize(issueType),
	}, "\x00")
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", hash[:])
}

func findingID(runID, fingerprint string) string {
	hash := sha256.Sum256([]byte(runID + "\x00" + fingerprint))
	return fmt.Sprintf("finding-%x", hash[:8])
}

func normalize(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
