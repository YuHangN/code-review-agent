package domain

import "time"

// Report 保存一次 Run 的权威 Markdown 产物及其完整性哈希。
type Report struct {
	RunID         string
	OutputPath    string
	Content       string
	ContentSHA256 string
	CreatedAt     time.Time
}
