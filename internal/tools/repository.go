package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/scm"
	"github.com/YuHangN/code-review-agent/internal/security"
)

var (
	ErrInvalidArguments = errors.New("invalid tool arguments")
	ErrSensitiveFile    = errors.New("sensitive repository file")
)

var searchHunkPattern = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// RepositoryReader 是固定 SHA 文件读取工具所需的最小 SCM 能力。
type RepositoryReader interface {
	ReadFile(ctx context.Context, ref scm.ChangeRef, sha, filePath string) ([]byte, error)
}

// RepositoryFileTool 读取一个固定 head SHA 下的文本文件并在返回前脱敏。
type RepositoryFileTool struct {
	reader  RepositoryReader
	ref     scm.ChangeRef
	headSHA string
}

func NewRepositoryFileTool(reader RepositoryReader, ref scm.ChangeRef, headSHA string) RepositoryFileTool {
	return RepositoryFileTool{reader: reader, ref: ref, headSHA: headSHA}
}

func (RepositoryFileTool) RequiredPermission() string { return "snapshot_read" }

func (RepositoryFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"仓库根目录下的相对文件路径"}},"required":["path"],"additionalProperties":false}`)
}

func (tool RepositoryFileTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments struct {
		Path string `json:"path"`
	}
	if tool.reader == nil || tool.ref.Provider == "" || tool.ref.Repository == "" || tool.ref.Number <= 0 || tool.headSHA == "" || decodeArguments(raw, &arguments) != nil || strings.TrimSpace(arguments.Path) == "" {
		return "", ErrInvalidArguments
	}
	if _, excluded, _ := security.NewSanitizer().SanitizeFile(arguments.Path, ""); excluded {
		return "", ErrSensitiveFile
	}
	content, err := tool.reader.ReadFile(ctx, tool.ref, tool.headSHA, arguments.Path)
	if err != nil {
		return "", fmt.Errorf("read pinned repository file: %w", err)
	}
	sanitized, excluded, redactions := security.NewSanitizer().SanitizeFile(arguments.Path, string(content))
	if excluded {
		return "", ErrSensitiveFile
	}
	result, err := json.Marshal(struct {
		Path       string `json:"path"`
		SHA        string `json:"sha"`
		Content    string `json:"content"`
		Redactions int    `json:"redactions"`
	}{Path: arguments.Path, SHA: tool.headSHA, Content: sanitized, Redactions: len(redactions)})
	if err != nil {
		return "", fmt.Errorf("encode repository file result: %w", err)
	}
	return string(result), nil
}

// SnapshotSearchTool 只搜索本次 Run 已固定并脱敏的 diff，不读取可变分支。
type SnapshotSearchTool struct {
	diff       string
	maxMatches int
}

func NewSnapshotSearchTool(diff string, maxMatches int) SnapshotSearchTool {
	sanitized := security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: diff})
	return SnapshotSearchTool{diff: sanitized.Snapshot.Diff, maxMatches: maxMatches}
}

func (SnapshotSearchTool) RequiredPermission() string { return "snapshot_read" }

func (SnapshotSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string","description":"要在固定 PR diff 中搜索的符号"}},"required":["symbol"],"additionalProperties":false}`)
}

type symbolMatch struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func (tool SnapshotSearchTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var arguments struct {
		Symbol string `json:"symbol"`
	}
	if tool.maxMatches <= 0 || decodeArguments(raw, &arguments) != nil {
		return "", ErrInvalidArguments
	}
	arguments.Symbol = strings.TrimSpace(arguments.Symbol)
	if len(arguments.Symbol) < 2 || len(arguments.Symbol) > 128 || strings.ContainsAny(arguments.Symbol, "\r\n") {
		return "", ErrInvalidArguments
	}
	matches := searchSnapshot(tool.diff, arguments.Symbol, tool.maxMatches)
	result, err := json.Marshal(struct {
		Symbol  string        `json:"symbol"`
		Matches []symbolMatch `json:"matches"`
	}{Symbol: arguments.Symbol, Matches: matches})
	if err != nil {
		return "", fmt.Errorf("encode symbol search result: %w", err)
	}
	return string(result), nil
}

func searchSnapshot(diff, symbol string, limit int) []symbolMatch {
	var matches []symbolMatch
	filePath := ""
	newLine := 0
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git a/") {
			parts := strings.SplitN(strings.TrimPrefix(line, "diff --git a/"), " b/", 2)
			if len(parts) == 2 {
				filePath = parts[1]
			}
			inHunk = false
			continue
		}
		if match := searchHunkPattern.FindStringSubmatch(line); match != nil {
			newLine, _ = strconv.Atoi(match[1])
			inHunk = true
			continue
		}
		if !inHunk || line == "" || strings.HasPrefix(line, `\ No newline`) {
			continue
		}
		if line[0] == '-' {
			continue
		}
		text := line
		if line[0] == '+' || line[0] == ' ' {
			text = line[1:]
		}
		if strings.Contains(text, symbol) && len(matches) < limit {
			matches = append(matches, symbolMatch{File: filePath, Line: newLine, Text: text})
		}
		newLine++
	}
	return matches
}

func decodeArguments(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrInvalidArguments
	}
	return nil
}
