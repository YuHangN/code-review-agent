package llm

import "context"

// ByteUpperBoundCounter 用 UTF-8 字节数作为保守 token 上界。
// 它不依赖特定模型 tokenizer，适合离线 Demo；真实 Provider 可替换为精确实现。
type ByteUpperBoundCounter struct{}

func (ByteUpperBoundCounter) CountInputTokens(_ context.Context, _ string, prompt string) (int64, error) {
	return int64(len([]byte(prompt))), nil
}
