package domain

import "time"

// CheckerStatus 表示一次仓库级静态检查是否还需要执行。
type CheckerStatus string

const (
	CheckerStatusPending           CheckerStatus = "pending"
	CheckerStatusRunning           CheckerStatus = "running"
	CheckerStatusCompleted         CheckerStatus = "completed"
	CheckerStatusFailedRecoverable CheckerStatus = "failed_recoverable"
)

// CheckerRun 是 go vet 或 staticcheck 的独立 checkpoint。
type CheckerRun struct {
	ID           string
	RunID        string
	Checker      string
	Status       CheckerStatus
	Attempt      int
	Command      []string
	ExitCode     int
	Output       string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CheckerDiagnostic 是静态检查工具映射到 PR 新增行后的结构化诊断。
type CheckerDiagnostic struct {
	ID           string
	RunID        string
	CheckerRunID string
	UnitID       string
	TraceID      string
	Checker      string
	File         string
	Line         int
	Column       int
	Code         string
	Message      string
	Severity     string
	CreatedAt    time.Time
}
