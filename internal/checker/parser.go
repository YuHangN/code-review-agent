// Package checker 在受限容器中运行可信静态检查器，并将诊断映射到 PR 新增行。
package checker

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

var (
	diagnosticPattern = regexp.MustCompile(`^(?:\./)?([^:]+\.go):(\d+):(\d+):\s*(.*?)(?:\s+\(([A-Z]+\d+)\))?$`)
	hunkPattern       = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

// ParsedDiagnostic 是尚未持久化的不可信 Checker 输出，进入其他边界前仍需脱敏和编码。
type ParsedDiagnostic struct {
	UnitID  string
	File    string
	Line    int
	Column  int
	Code    string
	Message string
}

// ParseDiagnostics 只保留能够精确映射到 Review Unit 新增行的 Go 诊断。
func ParseDiagnostics(checkerName, output string, units []domain.ReviewUnit) []ParsedDiagnostic {
	added := make(map[string]map[int]string)
	for _, unit := range units {
		for _, line := range addedLineNumbers(unit.DiffHunk) {
			if added[unit.FilePath] == nil {
				added[unit.FilePath] = make(map[int]string)
			}
			added[unit.FilePath][line] = unit.ID
		}
	}
	var result []ParsedDiagnostic
	for _, raw := range strings.Split(output, "\n") {
		match := diagnosticPattern.FindStringSubmatch(strings.TrimSpace(raw))
		if match == nil {
			continue
		}
		file := path.Clean(strings.TrimPrefix(match[1], "./"))
		if file == "." || strings.HasPrefix(file, "../") || strings.HasPrefix(file, "/") {
			continue
		}
		line, _ := strconv.Atoi(match[2])
		column, _ := strconv.Atoi(match[3])
		unitID := added[file][line]
		if unitID == "" {
			continue
		}
		code := match[5]
		if code == "" {
			code = checkerName
		}
		result = append(result, ParsedDiagnostic{UnitID: unitID, File: file, Line: line, Column: column, Code: code, Message: strings.TrimSpace(match[4])})
	}
	return result
}

func addedLineNumbers(diff string) []int {
	line, inHunk := 0, false
	var result []int
	for _, raw := range strings.Split(diff, "\n") {
		if match := hunkPattern.FindStringSubmatch(raw); match != nil {
			line, _ = strconv.Atoi(match[1])
			inHunk = true
			continue
		}
		if !inHunk || raw == "" || strings.HasPrefix(raw, `\ No newline`) {
			continue
		}
		switch raw[0] {
		case '+':
			result = append(result, line)
			line++
		case '-':
		default:
			line++
		}
	}
	return result
}
