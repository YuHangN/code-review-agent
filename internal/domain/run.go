package domain

import "time"

// RunStatus 表示一次 Review 在持久化状态机中的阶段。
type RunStatus string

const (
	RunStatusCreated     RunStatus = "created"
	RunStatusFetching    RunStatus = "fetching"
	RunStatusFetched     RunStatus = "fetched"
	RunStatusPlanned     RunStatus = "planned"
	RunStatusReviewing   RunStatus = "reviewing"
	RunStatusAggregating RunStatus = "aggregating"
	RunStatusReported    RunStatus = "reported"
	RunStatusPublishing  RunStatus = "publishing"
	RunStatusPublished   RunStatus = "published"
)

// UnitStatus 表示一个可独立恢复的 Review Unit 是否仍需处理。
type UnitStatus string

const (
	UnitStatusPending           UnitStatus = "pending"
	UnitStatusRunning           UnitStatus = "running"
	UnitStatusCompleted         UnitStatus = "completed"
	UnitStatusFailedRecoverable UnitStatus = "failed_recoverable"
	UnitStatusSkippedBudget     UnitStatus = "skipped_budget"
)

// Run 保存一次 PR/MR Review 的身份、状态和执行 lease。
type Run struct {
	ID                string
	SourceURL         string
	Provider          string
	Repository        string
	ChangeNumber      int
	Status            RunStatus
	BudgetLimitMicros int64
	LeaseOwner        string
	LeaseExpiresAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ReviewUnit 是一次 Review 中最小的 checkpoint 单元。
// 恢复时会复用 completed Unit，不会再次审查。
type ReviewUnit struct {
	ID        string
	RunID     string
	UnitKey   string
	FilePath  string
	Risk      string
	Status    UnitStatus
	Attempt   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChangeSnapshot 是一次 Review 固定下来的变更版本及其 diff。
// 相同 Run 不允许替换 Snapshot，避免恢复时混入后来 push 的代码。
type ChangeSnapshot struct {
	BaseSHA    string
	HeadSHA    string
	Diff       string
	DiffSHA256 string
	CreatedAt  time.Time
}
