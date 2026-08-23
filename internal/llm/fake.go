package llm

import (
	"context"
	"sync"
)

// FakeProvider 为自动化测试返回预设结果，不访问外部网络。
type FakeProvider struct {
	Response Response
	Err      error

	mu       sync.Mutex
	requests []GenerateRequest
}

func (provider *FakeProvider) Generate(_ context.Context, request GenerateRequest) (Response, error) {
	provider.mu.Lock()
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	if provider.Err != nil {
		return Response{}, provider.Err
	}
	return provider.Response, nil
}

// Requests 返回调用快照，避免测试直接读取内部切片产生数据竞争。
func (provider *FakeProvider) Requests() []GenerateRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]GenerateRequest(nil), provider.requests...)
}
