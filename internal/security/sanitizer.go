// Package security 提供进入后续 Review 流程前的内容脱敏边界。
package security

import (
	"crypto/sha256"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

var (
	diffHeaderPattern = regexp.MustCompile(`(?m)^diff --git a/([^\n]+) b/([^\n]+)\n?`)
	assignmentPattern = regexp.MustCompile(`(?im)(\b(api[_-]?key|password|token|secret)\b\s*[:=]\s*)(["']?)([^"'\s,]+)(["']?)`)
	bearerPattern     = regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~-]{16,})`)
	jwtPattern        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	knownTokenPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{16,})\b`)
)

// Sanitizer 将敏感文件和疑似凭据替换为可追踪但不可还原的占位符。
type Sanitizer struct{}

// Result 包含可安全继续处理的 Snapshot 及不含 secret 原文的脱敏摘要。
type Result struct {
	Snapshot      domain.ChangeSnapshot
	Redactions    []Redaction
	ExcludedFiles []string
}

// Redaction 记录一次脱敏的类别和占位符，不保存被替换的原始值。
type Redaction struct {
	Kind        string
	Placeholder string
}

// NewSanitizer 创建默认的 Secret Scanner。
func NewSanitizer() Sanitizer {
	return Sanitizer{}
}

// SanitizeSnapshot 排除敏感文件并脱敏 diff 中的疑似凭据。
func (Sanitizer) SanitizeSnapshot(input domain.ChangeSnapshot) Result {
	result := Result{Snapshot: input}
	result.Snapshot.Diff = sanitizeDiff(input.Diff, &result)
	diffHash := sha256.Sum256([]byte(result.Snapshot.Diff))
	result.Snapshot.DiffSHA256 = fmt.Sprintf("%x", diffHash)
	return result
}

func sanitizeDiff(diff string, result *Result) string {
	indices := diffHeaderPattern.FindAllStringSubmatchIndex(diff, -1)
	if len(indices) == 0 {
		return redactText(diff, result)
	}

	var sanitized strings.Builder
	for index, match := range indices {
		blockEnd := len(diff)
		if index+1 < len(indices) {
			blockEnd = indices[index+1][0]
		}
		filePath := diff[match[2]:match[3]]
		if isSensitivePath(filePath) {
			sanitized.WriteString(diff[match[0]:match[1]])
			sanitized.WriteString(fmt.Sprintf("<REDACTED_FILE:%s>\n", filePath))
			result.ExcludedFiles = append(result.ExcludedFiles, filePath)
			continue
		}
		sanitized.WriteString(redactText(diff[match[0]:blockEnd], result))
	}
	return sanitized.String()
}

func isSensitivePath(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx", ".crt", ".cer"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return strings.Contains(base, "credential") || strings.Contains(base, "secret") || base == "id_rsa"
}

func redactText(text string, result *Result) string {
	text = assignmentPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := assignmentPattern.FindStringSubmatch(match)
		return parts[1] + parts[3] + nextPlaceholder(result, strings.ToUpper(strings.ReplaceAll(parts[2], "-", "_"))) + parts[5]
	})
	text = bearerPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := bearerPattern.FindStringSubmatch(match)
		return parts[1] + nextPlaceholder(result, "BEARER_TOKEN")
	})
	text = jwtPattern.ReplaceAllStringFunc(text, func(string) string {
		return nextPlaceholder(result, "JWT")
	})
	return knownTokenPattern.ReplaceAllStringFunc(text, func(string) string {
		return nextPlaceholder(result, "TOKEN")
	})
}

func nextPlaceholder(result *Result, kind string) string {
	placeholder := fmt.Sprintf("<REDACTED:%s:%d>", kind, len(result.Redactions)+1)
	result.Redactions = append(result.Redactions, Redaction{Kind: kind, Placeholder: placeholder})
	return placeholder
}
