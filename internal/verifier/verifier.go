// Package verifier 将 Reviewer 产生的候选问题转换为有证据等级的最终 Finding。
package verifier

import (
	"crypto/sha256"
	"fmt"
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

// Rule 是可扩展的确定性验证规则；新规则不需要修改 Verifier 主流程。
type Rule interface {
	Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit) RuleResult
}

// Verifier 按注册顺序执行规则，首个命中结果即可确认候选问题。
type Verifier struct {
	rules []Rule
}

// NewDefault 创建首版内置规则集合。
func NewDefault() Verifier {
	return Verifier{rules: []Rule{redactedSecretAssignmentRule{}}}
}

// Verify 根据确定性证据分类；未命中任何规则的候选只能是 advisory。
func (verifier Verifier) Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit, now time.Time) domain.VerifiedFinding {
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
		result := rule.Verify(candidate, unit)
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
func (redactedSecretAssignmentRule) Verify(candidate domain.CandidateFindingRecord, unit domain.ReviewUnit) RuleResult {
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
	identity := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(candidate.File)),
		strconv.Itoa(candidate.Line),
		strings.ToLower(strings.TrimSpace(candidate.Category)),
		normalize(candidate.Title),
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
