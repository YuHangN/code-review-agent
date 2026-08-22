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

// ReviewData 是 Reviewer Prompt 中允许注入的结构化变量。
type ReviewData struct {
	MaxFindings int
	FilePath    string
	Risk        string
	Diff        string
}

// RenderReview 使用内嵌模板生成 Prompt，因此 CLI 不依赖运行时工作目录。
func RenderReview(data ReviewData) (string, error) {
	var output bytes.Buffer
	if err := reviewTemplate.ExecuteTemplate(&output, "default.tmpl", data); err != nil {
		return "", fmt.Errorf("execute review prompt template: %w", err)
	}
	return output.String(), nil
}
