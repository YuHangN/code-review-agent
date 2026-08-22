// Package planner 将固定 Snapshot 切分为可独立恢复的 Review Unit。
package planner

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

var hunkPattern = regexp.MustCompile(`(?m)^@@ -[^ ]+ \+(\d+)(?:,(\d+))? @@`)

// Request 是生成 Unit 所需的不可变 Snapshot 输入。
type Request struct {
	RunID   string
	HeadSHA string
	Diff    string
	Now     time.Time
}

// Planner 按文件和 hunk 生成稳定的 Review Unit。
type Planner struct{}

// New 创建一个不依赖外部状态的 Planner。
func New() Planner { return Planner{} }

// Plan 不依赖网络或模型；同一输入始终产生相同的 Unit key 和顺序。
func (Planner) Plan(request Request) []domain.ReviewUnit {
	blocks := strings.Split(request.Diff, "diff --git a/")
	var units []domain.ReviewUnit
	for _, block := range blocks[1:] {
		line, body, _ := strings.Cut(block, "\n")
		filePath, _, ok := strings.Cut(line, " b/")
		if !ok || shouldSkip(filePath) || strings.Contains(body, "<REDACTED_FILE:") {
			continue
		}
		matches := hunkPattern.FindAllStringSubmatchIndex(body, -1)
		if len(matches) == 0 {
			units = append(units, newUnit(request, filePath, 0, 0, body))
			continue
		}
		for index, match := range matches {
			end := len(body)
			if index+1 < len(matches) {
				end = matches[index+1][0]
			}
			start := parseNumber(body[match[2]:match[3]])
			count := 1
			if match[4] >= 0 {
				count = parseNumber(body[match[4]:match[5]])
			}
			units = append(units, newUnit(request, filePath, start, start+count-1, body[match[0]:end]))
		}
	}
	return units
}

func newUnit(request Request, filePath string, start, end int, content string) domain.ReviewUnit {
	keyInput := fmt.Sprintf("%s\x00%s\x00%d-%d", request.HeadSHA, filePath, start, end)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(keyInput)))
	return domain.ReviewUnit{
		ID:        "unit-" + key[:16],
		RunID:     request.RunID,
		UnitKey:   key,
		FilePath:  filePath,
		StartLine: start,
		EndLine:   end,
		DiffHunk:  content,
		Risk:      risk(filePath, content),
		Status:    domain.UnitStatusPending,
		CreatedAt: request.Now,
		UpdatedAt: request.Now,
	}
}

func shouldSkip(filePath string) bool {
	base := strings.ToLower(filePath)
	return strings.HasPrefix(base, "vendor/") || strings.HasSuffix(base, ".lock") || base == "go.sum" || strings.HasSuffix(base, ".min.js")
}

func risk(filePath, content string) string {
	input := strings.ToLower(filePath + "\n" + content)
	for _, marker := range []string{"auth", "oauth", "password", "token", "api_key", "sql", "select ", "insert ", "go func", "goroutine", "config", "yaml"} {
		if strings.Contains(input, marker) {
			return "high"
		}
	}
	return "medium"
}

func parseNumber(value string) int {
	number, _ := strconv.Atoi(value)
	return number
}
