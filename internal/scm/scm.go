// Package scm 定义代码托管平台适配器的通用模型。
package scm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

// ArchiveAdapter 是需要固定 SHA 完整源码的仓库级检查器所依赖的可选能力。
type ArchiveAdapter interface {
	OpenArchive(ctx context.Context, ref ChangeRef, sha string) (io.ReadCloser, error)
}

var ErrUnsupportedURL = errors.New("unsupported SCM change URL")

// ChangeRef 使用平台无关的字段标识一次 PR 或 MR。
type ChangeRef struct {
	Provider   string
	Repository string
	Number     int
}

// Adapter 屏蔽不同代码托管平台在 URL、快照和文件读取上的 API 差异。
type Adapter interface {
	Provider() string
	ParseURL(rawURL string) (ChangeRef, error)
	Fetch(ctx context.Context, ref ChangeRef) (domain.ChangeSnapshot, error)
	ReadFile(ctx context.Context, ref ChangeRef, sha, filePath string) ([]byte, error)
}

// ResolvedChange 是一次 URL 解析后确定的平台引用和对应 Adapter。
type ResolvedChange struct {
	Ref     ChangeRef
	Adapter Adapter
}

// Registry 是 SCM Adapter 的声明式注册表，主流程无需判断具体平台。
type Registry struct {
	ordered   []Adapter
	providers map[string]Adapter
}

// NewRegistry 注册可用平台；同一个 provider 不能重复注册。
func NewRegistry(adapters ...Adapter) (Registry, error) {
	registry := Registry{
		ordered:   make([]Adapter, 0, len(adapters)),
		providers: make(map[string]Adapter, len(adapters)),
	}
	for _, adapter := range adapters {
		if adapter == nil || strings.TrimSpace(adapter.Provider()) == "" {
			return Registry{}, fmt.Errorf("register SCM adapter: invalid provider")
		}
		provider := adapter.Provider()
		if _, exists := registry.providers[provider]; exists {
			return Registry{}, fmt.Errorf("register SCM adapter: duplicate provider %q", provider)
		}
		registry.ordered = append(registry.ordered, adapter)
		registry.providers[provider] = adapter
	}
	return registry, nil
}

// ResolveURL 让已注册 Adapter 识别 URL，并返回可持久化的通用引用。
func (registry Registry) ResolveURL(rawURL string) (ResolvedChange, error) {
	for _, adapter := range registry.ordered {
		ref, err := adapter.ParseURL(rawURL)
		if errors.Is(err, ErrUnsupportedURL) {
			continue
		}
		if err != nil {
			return ResolvedChange{}, err
		}
		return ResolvedChange{Ref: ref, Adapter: adapter}, nil
	}
	return ResolvedChange{}, ErrUnsupportedURL
}

// Adapter 根据数据库保存的 provider 恢复对应平台实现。
func (registry Registry) Adapter(provider string) (Adapter, error) {
	adapter, exists := registry.providers[provider]
	if !exists {
		return nil, fmt.Errorf("SCM adapter %q is not registered", provider)
	}
	return adapter, nil
}
