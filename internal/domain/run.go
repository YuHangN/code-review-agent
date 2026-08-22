package domain

import "time"

type RunStatus string

const (
	RunStatusCreated     RunStatus = "created"
	RunStatusFetching    RunStatus = "fetching"
	RunStatusFetched     RunStatus = "fetched"
	RunStatusPlanning    RunStatus = "planning"
	RunStatusReviewing   RunStatus = "reviewing"
	RunStatusAggregating RunStatus = "aggregating"
	RunStatusReported    RunStatus = "reported"
	RunStatusPublishing  RunStatus = "publishing"
	RunStatusPublished   RunStatus = "published"
)

type UnitStatus string

const (
	UnitStatusPending           UnitStatus = "pending"
	UnitStatusRunning           UnitStatus = "running"
	UnitStatusCompleted         UnitStatus = "completed"
	UnitStatusFailedRecoverable UnitStatus = "failed_recoverable"
	UnitStatusSkippedBudget     UnitStatus = "skipped_budget"
)

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
