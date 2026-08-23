package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/scm"
	"github.com/YuHangN/code-review-agent/internal/tools"
)

func TestRepositoryFileReadsPinnedSHAAndSanitizesContent(t *testing.T) {
	reader := &recordingReader{content: []byte("package auth\nconst api_key = \"sk-1234567890abcdef\"\n")}
	tool := tools.NewRepositoryFileTool(reader, scm.ChangeRef{Provider: "github", Repository: "acme/payments", Number: 42}, "head-sha")

	content, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"internal/auth/token.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if reader.sha != "head-sha" || reader.path != "internal/auth/token.go" {
		t.Fatalf("read sha/path = %q %q", reader.sha, reader.path)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, "sk-1234567890abcdef") || !strings.Contains(result.Content, "<REDACTED:API_KEY:1>") {
		t.Fatalf("tool content was not sanitized: %q", result.Content)
	}
}

func TestRepositoryFileRejectsSensitivePathBeforeReading(t *testing.T) {
	reader := &recordingReader{content: []byte("SECRET=value")}
	tool := tools.NewRepositoryFileTool(reader, scm.ChangeRef{Provider: "github", Repository: "acme/payments", Number: 42}, "head-sha")

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"config/.env"}`))
	if err == nil || !strings.Contains(err.Error(), "sensitive repository file") {
		t.Fatalf("execute error = %v", err)
	}
	if reader.path != "" {
		t.Fatal("sensitive path reached repository reader")
	}
}

func TestSnapshotSearchReturnsOnlyBoundedImmutableDiffMatches(t *testing.T) {
	diff := "diff --git a/internal/auth/token.go b/internal/auth/token.go\n@@ -10,2 +10,3 @@\n func Handler() {\n+\tvalidateToken(token)\n }\n"
	tool := tools.NewSnapshotSearchTool(diff, 10)

	content, err := tool.Execute(context.Background(), json.RawMessage(`{"symbol":"validateToken"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `"file":"internal/auth/token.go"`) || !strings.Contains(content, `"line":11`) || !strings.Contains(content, "validateToken(token)") {
		t.Fatalf("search content = %s", content)
	}
}

type recordingReader struct {
	content []byte
	sha     string
	path    string
}

func (reader *recordingReader) ReadFile(_ context.Context, _ scm.ChangeRef, sha, filePath string) ([]byte, error) {
	reader.sha = sha
	reader.path = filePath
	return append([]byte(nil), reader.content...), nil
}
