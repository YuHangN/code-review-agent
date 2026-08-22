// Package scm 定义代码托管平台适配器的通用模型。
package scm

import (
	"context"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

// PullRequestRef 唯一标识一个 GitHub Pull Request。
type PullRequestRef struct {
	Owner      string
	Repository string
	Number     int
}

// Adapter 提供固定版本的变更快照，屏蔽各托管平台的 API 差异。
type Adapter interface {
	Fetch(ctx context.Context, ref PullRequestRef) (domain.ChangeSnapshot, error)
}
