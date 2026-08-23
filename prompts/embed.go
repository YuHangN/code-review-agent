// Package prompts 保存并渲染编译进二进制的 Prompt 模板。
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed review/default.tmpl
var files embed.FS

var reviewTemplate = template.Must(
	template.New("review").Option("missingkey=error").ParseFS(files, "review/default.tmpl"),
)

//go:embed review/agent.tmpl
var agentTemplateSource string

var agentReviewTemplate = template.Must(
	template.New("agent-review").Option("missingkey=error").Parse(agentTemplateSource),
)

// ReviewData 是 Reviewer Prompt 中允许注入的结构化变量。
type ReviewData struct {
	MaxFindings      int
	FilePath         string
	Risk             string
	Diff             string
	KnownDiagnostics string
}

// AgentReviewData 在基础 Review 输入上增加 Registry 已授权的工具定义。
type AgentReviewData struct {
	ReviewData
	ToolDefinitions string
}

// RenderReview 使用内嵌模板生成 Prompt，因此 CLI 不依赖运行时工作目录。
func RenderReview(data ReviewData) (string, error) {
	var output bytes.Buffer
	if err := reviewTemplate.ExecuteTemplate(&output, "default.tmpl", data); err != nil {
		return "", fmt.Errorf("execute review prompt template: %w", err)
	}
	return output.String(), nil
}

// RenderAgentReview 渲染结构化 Tool-Calling Loop 的首轮 Prompt。
func RenderAgentReview(data AgentReviewData) (string, error) {
	var output bytes.Buffer
	if err := agentReviewTemplate.Execute(&output, data); err != nil {
		return "", fmt.Errorf("execute agent review prompt template: %w", err)
	}
	return output.String(), nil
}
