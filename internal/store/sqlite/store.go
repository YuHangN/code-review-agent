package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/migrations"
	_ "modernc.org/sqlite"
)

var (
	ErrUnitNotRunnable = errors.New("review unit is not runnable")
	// ErrRunNotReady 表示 Run 仍有非终态 Unit，不能进入聚合阶段。
	ErrRunNotReady = errors.New("run is not ready for aggregation")
	// ErrRunNotAggregating 表示最终 Finding 只能在聚合阶段写入。
	ErrRunNotAggregating = errors.New("run is not aggregating")
	// ErrRunNotReportable 表示只有 aggregating/reported Run 可以保存报告。
	ErrRunNotReportable = errors.New("run is not reportable")
	// ErrRunNotFetchable 表示只有 created/fetching Run 可以进入或完成抓取阶段。
	ErrRunNotFetchable = errors.New("run is not fetchable")
	// ErrInvalidSnapshot 表示抓取结果缺少固定版本或完整性信息。
	ErrInvalidSnapshot = errors.New("invalid change snapshot")
	// ErrAgentStepConflict 表示同一 Unit/round 已存在不同的 Agent 证据。
	ErrAgentStepConflict = errors.New("agent step checkpoint conflicts with existing record")
)

var (
	// ErrLeaseHeld 表示当前 Run 的有效 lease 已被其他执行者持有。
	ErrLeaseHeld = errors.New("run lease is held by another owner")
	// ErrLeaseOwnerRequired 表示领取 lease 时缺少唯一执行者标识。
	ErrLeaseOwnerRequired = errors.New("lease owner is required")
)

// Store 将 Review 状态持久化到本地 SQLite。
type Store struct {
	db *sql.DB
}

// Options 控制 SQLite 连接的运行时参数。
type Options struct {
	BusyTimeout time.Duration
}

// Open 配置 SQLite 的并发与持久化选项，并执行初始 schema migration。
// DSN 中的 pragma 会应用到 database/sql 新建的每条物理连接。
func Open(ctx context.Context, path string, options Options) (*Store, error) {
	if options.BusyTimeout < time.Millisecond {
		return nil, fmt.Errorf("SQLite busy timeout must be at least 1ms")
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)", path, options.BusyTimeout.Milliseconds()))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	store := &Store{db: db}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("begin migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("create migration ledger: %w", err)
	}
	for _, migration := range migrations.All {
		var applied int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, migration.Version).Scan(&applied)
		if err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("check migration %d: %w", migration.Version, err)
		}
		if applied != 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("run migration %d: %w", migration.Version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, migration.Version, timeText(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return nil, fmt.Errorf("record migration %d: %w", migration.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("commit migration: %w", err)
	}
	return store, nil
}

// Close 释放数据库连接池。
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateRun 原子写入一个 Run 及其全部 Review Unit。
// 任一 Unit 写入失败时，Run 和所有 Unit 都不会提交。
func (s *Store) CreateRun(ctx context.Context, run domain.Run, units []domain.ReviewUnit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create run: %w", err)
	}
	defer tx.Rollback()

	if err := insertRun(ctx, tx, run, units); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create run: %w", err)
	}
	return nil
}

// CreateRunWithSnapshot 原子写入 Run、Unit 和首次抓取的 Snapshot。
func (s *Store) CreateRunWithSnapshot(ctx context.Context, run domain.Run, units []domain.ReviewUnit, snapshot domain.ChangeSnapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create fetched run: %w", err)
	}
	defer tx.Rollback()

	if err := insertRun(ctx, tx, run, units); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO change_snapshots (run_id, base_sha, head_sha, diff, diff_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		run.ID, snapshot.BaseSHA, snapshot.HeadSHA, snapshot.Diff, snapshot.DiffSHA256, timeText(snapshot.CreatedAt),
	); err != nil {
		return fmt.Errorf("insert change snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create fetched run: %w", err)
	}
	return nil
}

func insertRun(ctx context.Context, tx *sql.Tx, run domain.Run, units []domain.ReviewUnit) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runs (
			id, source_url, provider, repository, change_number, status, budget_limit_micros,
			lease_owner, lease_expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.SourceURL, run.Provider, run.Repository, run.ChangeNumber, run.Status,
		run.BudgetLimitMicros, nullableString(run.LeaseOwner), nullableTime(run.LeaseExpiresAt),
		timeText(run.CreatedAt), timeText(run.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}

	for _, unit := range units {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO review_units (
				id, run_id, unit_key, file_path, start_line, end_line, diff_hunk,
				risk, status, attempt, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			unit.ID, unit.RunID, unit.UnitKey, unit.FilePath, unit.StartLine, unit.EndLine, unit.DiffHunk, unit.Risk, unit.Status,
			unit.Attempt, timeText(unit.CreatedAt), timeText(unit.UpdatedAt),
		); err != nil {
			return fmt.Errorf("insert review unit: %w", err)
		}
	}
	return nil
}

// GetRun 读取一个 Run 的持久化状态和当前 lease。
func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source_url, provider, repository, change_number, status, budget_limit_micros,
		       COALESCE(lease_owner, ''), lease_expires_at, created_at, updated_at
		FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// BeginFetch 将新建 Run 推进到 fetching；对已经处于 fetching 的 Run 可安全重复调用。
func (s *Store) BeginFetch(ctx context.Context, runID string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		domain.RunStatusFetching, timeText(now), runID,
		domain.RunStatusCreated, domain.RunStatusFetching,
	)
	if err != nil {
		return fmt.Errorf("begin fetching run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read begin fetching result: %w", err)
	}
	if changed != 1 {
		return ErrRunNotFetchable
	}
	return nil
}

// SaveFetchedSnapshot 原子保存脱敏 Snapshot，并将 Run 从 fetching 推进到 fetched。
func (s *Store) SaveFetchedSnapshot(ctx context.Context, runID string, snapshot domain.ChangeSnapshot, now time.Time) error {
	if runID == "" || snapshot.BaseSHA == "" || snapshot.HeadSHA == "" || snapshot.DiffSHA256 == "" || snapshot.CreatedAt.IsZero() || now.IsZero() {
		return ErrInvalidSnapshot
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save fetched snapshot: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		domain.RunStatusFetched, timeText(now), runID, domain.RunStatusFetching,
	)
	if err != nil {
		return fmt.Errorf("complete fetching run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read complete fetching result: %w", err)
	}
	if changed != 1 {
		return ErrRunNotFetchable
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO change_snapshots (run_id, base_sha, head_sha, diff, diff_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		runID, snapshot.BaseSHA, snapshot.HeadSHA, snapshot.Diff, snapshot.DiffSHA256, timeText(snapshot.CreatedAt),
	); err != nil {
		return fmt.Errorf("insert fetched snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fetched snapshot: %w", err)
	}
	return nil
}

// ListUnits 按稳定的 unit key 顺序返回 Run 的 Unit，保证恢复结果可预测。
func (s *Store) ListUnits(ctx context.Context, runID string) ([]domain.ReviewUnit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, unit_key, file_path, start_line, end_line, diff_hunk,
		       risk, status, attempt, created_at, updated_at
		FROM review_units WHERE run_id = ? ORDER BY unit_key`, runID)
	if err != nil {
		return nil, fmt.Errorf("list review units: %w", err)
	}
	defer rows.Close()

	var units []domain.ReviewUnit
	for rows.Next() {
		var unit domain.ReviewUnit
		var createdAt, updatedAt string
		if err := rows.Scan(
			&unit.ID, &unit.RunID, &unit.UnitKey, &unit.FilePath, &unit.StartLine, &unit.EndLine, &unit.DiffHunk, &unit.Risk,
			&unit.Status, &unit.Attempt, &createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan review unit: %w", err)
		}
		unit.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse review unit created_at: %w", err)
		}
		unit.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse review unit updated_at: %w", err)
		}
		units = append(units, unit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate review units: %w", err)
	}
	return units, nil
}

// StartReviewUnit 校验 Run lease 后，原子推进 Run 和 Unit 状态并增加 attempt。
func (s *Store) StartReviewUnit(ctx context.Context, unitID, owner string, now time.Time) (domain.ReviewUnit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("begin start review unit: %w", err)
	}
	defer tx.Rollback()
	var currentOwner string
	var leaseExpiresAt sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(r.lease_owner, ''), r.lease_expires_at
		FROM runs r JOIN review_units u ON u.run_id = r.id
		WHERE u.id = ?`, unitID,
	).Scan(&currentOwner, &leaseExpiresAt)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("read review unit lease: %w", err)
	}
	if currentOwner != owner || !leaseExpiresAt.Valid {
		return domain.ReviewUnit{}, ErrLeaseHeld
	}
	expiresAt, err := parseTime(leaseExpiresAt.String)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("parse review unit lease expiry: %w", err)
	}
	if !expiresAt.After(now) {
		return domain.ReviewUnit{}, ErrLeaseHeld
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE review_units SET status = ?, attempt = attempt + 1, updated_at = ?
		WHERE id = ? AND status IN (?, ?, ?)`,
		domain.UnitStatusRunning, timeText(now), unitID,
		domain.UnitStatusPending, domain.UnitStatusRunning, domain.UnitStatusFailedRecoverable,
	)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("start review unit: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("read start review unit result: %w", err)
	}
	if changed != 1 {
		return domain.ReviewUnit{}, ErrUnitNotRunnable
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?
		WHERE id = (SELECT run_id FROM review_units WHERE id = ?)
		  AND status IN (?, ?)`,
		domain.RunStatusReviewing, timeText(now), unitID,
		domain.RunStatusPlanned, domain.RunStatusReviewing,
	)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("start reviewing run: %w", err)
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("read reviewing run result: %w", err)
	}
	if changed != 1 {
		return domain.ReviewUnit{}, ErrUnitNotRunnable
	}
	if err := tx.Commit(); err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("commit start review unit: %w", err)
	}
	return s.getReviewUnit(ctx, unitID)
}

func (s *Store) getReviewUnit(ctx context.Context, unitID string) (domain.ReviewUnit, error) {
	var unit domain.ReviewUnit
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, unit_key, file_path, start_line, end_line, diff_hunk,
		       risk, status, attempt, created_at, updated_at
		FROM review_units WHERE id = ?`, unitID,
	).Scan(
		&unit.ID, &unit.RunID, &unit.UnitKey, &unit.FilePath, &unit.StartLine, &unit.EndLine, &unit.DiffHunk,
		&unit.Risk, &unit.Status, &unit.Attempt, &createdAt, &updatedAt,
	)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("get review unit: %w", err)
	}
	unit.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("parse review unit created_at: %w", err)
	}
	unit.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return domain.ReviewUnit{}, fmt.Errorf("parse review unit updated_at: %w", err)
	}
	return unit, nil
}

// GetReviewUnit 返回 Trace 所关联的不可变 Unit 输入。
func (s *Store) GetReviewUnit(ctx context.Context, unitID string) (domain.ReviewUnit, error) {
	return s.getReviewUnit(ctx, unitID)
}

// CompleteReviewUnit 在一个事务中保存 trace、候选问题和 completed checkpoint。
func (s *Store) CompleteReviewUnit(ctx context.Context, trace domain.ReviewTrace, findings []domain.CandidateFindingRecord, owner string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin complete review unit: %w", err)
	}
	defer tx.Rollback()
	if err := insertReviewTrace(ctx, tx, trace); err != nil {
		return err
	}
	for _, finding := range findings {
		evidenceJSON, err := json.Marshal(finding.Evidence)
		if err != nil {
			return fmt.Errorf("marshal finding evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO candidate_findings (
				id, run_id, unit_id, trace_id, detector, category, severity, file_path,
				line, title, explanation, evidence_json, suggestion, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			finding.ID, finding.RunID, finding.UnitID, finding.TraceID, finding.Detector,
			finding.Category, finding.Severity, finding.File, finding.Line, finding.Title,
			finding.Explanation, string(evidenceJSON), finding.Suggestion, timeText(finding.CreatedAt),
		); err != nil {
			return fmt.Errorf("insert candidate finding: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE review_units SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?
		  AND EXISTS (SELECT 1 FROM runs WHERE runs.id = review_units.run_id AND lease_owner = ?)`,
		domain.UnitStatusCompleted, timeText(now), trace.UnitID, domain.UnitStatusRunning, owner,
	)
	if err != nil {
		return fmt.Errorf("complete review unit: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read complete review unit result: %w", err)
	}
	if changed != 1 {
		return ErrLeaseHeld
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit complete review unit: %w", err)
	}
	return nil
}

// FinishReviewUnit 保存无 finding 的失败或预算跳过 trace，并推进 Unit 状态。
func (s *Store) FinishReviewUnit(ctx context.Context, trace domain.ReviewTrace, status domain.UnitStatus, owner string, now time.Time) error {
	if status != domain.UnitStatusFailedRecoverable && status != domain.UnitStatusSkippedBudget {
		return fmt.Errorf("unsupported review unit finish status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finish review unit: %w", err)
	}
	defer tx.Rollback()
	if err := insertReviewTrace(ctx, tx, trace); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE review_units SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?
		  AND EXISTS (SELECT 1 FROM runs WHERE runs.id = review_units.run_id AND lease_owner = ?)`,
		status, timeText(now), trace.UnitID, domain.UnitStatusRunning, owner,
	)
	if err != nil {
		return fmt.Errorf("finish review unit: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read finish review unit result: %w", err)
	}
	if changed != 1 {
		return ErrLeaseHeld
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finish review unit: %w", err)
	}
	return nil
}

// AdvanceRunToAggregating 在当前 lease 仍有效且所有 Unit 都是终态时推进 Run。
// completed 和 skipped_budget 都是终态；可恢复失败必须留在 reviewing 等待 resume。
func (s *Store) AdvanceRunToAggregating(ctx context.Context, runID, owner string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?
		WHERE id = ?
		  AND status IN (?, ?)
		  AND lease_owner = ?
		  AND lease_expires_at > ?
		  AND NOT EXISTS (
			SELECT 1 FROM review_units
			WHERE run_id = runs.id AND status NOT IN (?, ?)
		  )`,
		domain.RunStatusAggregating, timeText(now), runID,
		domain.RunStatusPlanned, domain.RunStatusReviewing,
		owner, timeText(now), domain.UnitStatusCompleted, domain.UnitStatusSkippedBudget,
	)
	if err != nil {
		return fmt.Errorf("advance run to aggregating: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read aggregating result: %w", err)
	}
	if changed == 1 {
		return nil
	}

	var currentOwner string
	var leaseExpiresAt sql.NullString
	var nonTerminal int
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(lease_owner, ''), lease_expires_at,
		       (SELECT COUNT(*) FROM review_units WHERE run_id = runs.id AND status NOT IN (?, ?))
		FROM runs WHERE id = ?`,
		domain.UnitStatusCompleted, domain.UnitStatusSkippedBudget, runID,
	).Scan(&currentOwner, &leaseExpiresAt, &nonTerminal)
	if err != nil {
		return fmt.Errorf("read aggregating preconditions: %w", err)
	}
	if currentOwner != owner || !leaseExpiresAt.Valid {
		return ErrLeaseHeld
	}
	expiresAt, err := parseTime(leaseExpiresAt.String)
	if err != nil {
		return fmt.Errorf("parse aggregating lease expiry: %w", err)
	}
	if !expiresAt.After(now) {
		return ErrLeaseHeld
	}
	if nonTerminal > 0 {
		return ErrRunNotReady
	}
	return ErrRunNotReady
}

func insertReviewTrace(ctx context.Context, tx *sql.Tx, trace domain.ReviewTrace) error {
	rejectionsJSON, err := json.Marshal(trace.Rejections)
	if err != nil {
		return fmt.Errorf("marshal trace rejections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_traces (
			id, run_id, unit_id, call_id, detector, status, prompt, response,
			rejections_json, error_message, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		trace.ID, trace.RunID, trace.UnitID, trace.CallID, trace.Detector, trace.Status,
		trace.Prompt, trace.Response, string(rejectionsJSON), trace.ErrorMessage, timeText(trace.CreatedAt),
	); err != nil {
		return fmt.Errorf("insert review trace: %w", err)
	}
	return nil
}

// SaveAgentStep 以 unit_id + round 为幂等键保存一轮脱敏 Agent 证据。
func (s *Store) SaveAgentStep(ctx context.Context, step domain.AgentStep, owner string, now time.Time) error {
	if step.RunID == "" || step.UnitID == "" || step.Round <= 0 || step.ModelCallID == "" || step.Prompt == "" || step.Response == "" || step.CreatedAt.IsZero() || owner == "" || now.IsZero() {
		return fmt.Errorf("invalid agent step")
	}
	toolCallsJSON, err := json.Marshal(step.ToolCalls)
	if err != nil {
		return fmt.Errorf("marshal agent tool calls: %w", err)
	}
	toolResultsJSON, err := json.Marshal(step.ToolResults)
	if err != nil {
		return fmt.Errorf("marshal agent tool results: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save agent step: %w", err)
	}
	defer tx.Rollback()
	var currentOwner string
	var leaseExpiresAt sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(r.lease_owner, ''), r.lease_expires_at
		FROM runs r
		JOIN review_units u ON u.run_id = r.id
		WHERE r.id = ? AND u.id = ?`, step.RunID, step.UnitID,
	).Scan(&currentOwner, &leaseExpiresAt); err != nil {
		return fmt.Errorf("read agent step lease: %w", err)
	}
	if currentOwner != owner || !leaseExpiresAt.Valid {
		return ErrLeaseHeld
	}
	expiresAt, err := parseTime(leaseExpiresAt.String)
	if err != nil {
		return fmt.Errorf("parse agent step lease expiry: %w", err)
	}
	if !expiresAt.After(now) {
		return ErrLeaseHeld
	}
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO agent_steps (
			unit_id, run_id, round, model_call_id, prompt, response,
			tool_calls_json, tool_results_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		step.UnitID, step.RunID, step.Round, step.ModelCallID, step.Prompt, step.Response,
		string(toolCallsJSON), string(toolResultsJSON), timeText(step.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert agent step: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read agent step insert result: %w", err)
	}
	if inserted == 1 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit agent step: %w", err)
		}
		return nil
	}
	var existing domain.AgentStep
	var existingCalls, existingResults string
	err = tx.QueryRowContext(ctx, `
		SELECT run_id, unit_id, round, model_call_id, prompt, response,
		       tool_calls_json, tool_results_json
		FROM agent_steps WHERE unit_id = ? AND round = ?`, step.UnitID, step.Round,
	).Scan(
		&existing.RunID, &existing.UnitID, &existing.Round, &existing.ModelCallID,
		&existing.Prompt, &existing.Response, &existingCalls, &existingResults,
	)
	if err != nil {
		return fmt.Errorf("read existing agent step: %w", err)
	}
	if existing.RunID == step.RunID && existing.UnitID == step.UnitID && existing.Round == step.Round && existing.ModelCallID == step.ModelCallID && existing.Prompt == step.Prompt && existing.Response == step.Response && existingCalls == string(toolCallsJSON) && existingResults == string(toolResultsJSON) {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit existing agent step: %w", err)
		}
		return nil
	}
	return ErrAgentStepConflict
}

// ListAgentSteps 按 round 返回 Unit 已完成的 Agent 轮次，用于恢复和 Trace。
func (s *Store) ListAgentSteps(ctx context.Context, unitID string) ([]domain.AgentStep, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, unit_id, round, model_call_id, prompt, response,
		       tool_calls_json, tool_results_json, created_at
		FROM agent_steps WHERE unit_id = ? ORDER BY round`, unitID)
	if err != nil {
		return nil, fmt.Errorf("list agent steps: %w", err)
	}
	defer rows.Close()
	var steps []domain.AgentStep
	for rows.Next() {
		var step domain.AgentStep
		var toolCallsJSON, toolResultsJSON, createdAt string
		if err := rows.Scan(
			&step.RunID, &step.UnitID, &step.Round, &step.ModelCallID, &step.Prompt,
			&step.Response, &toolCallsJSON, &toolResultsJSON, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent step: %w", err)
		}
		if err := json.Unmarshal([]byte(toolCallsJSON), &step.ToolCalls); err != nil {
			return nil, fmt.Errorf("parse agent tool calls: %w", err)
		}
		if err := json.Unmarshal([]byte(toolResultsJSON), &step.ToolResults); err != nil {
			return nil, fmt.Errorf("parse agent tool results: %w", err)
		}
		step.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse agent step created_at: %w", err)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent steps: %w", err)
	}
	return steps, nil
}

// ListCandidateFindings 返回一个 Run 尚待 Verifier 处理的候选问题。
func (s *Store) ListCandidateFindings(ctx context.Context, runID string) ([]domain.CandidateFindingRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, unit_id, trace_id, detector, category, severity, file_path,
		       line, title, explanation, evidence_json, suggestion, created_at
		FROM candidate_findings WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list candidate findings: %w", err)
	}
	defer rows.Close()
	var findings []domain.CandidateFindingRecord
	for rows.Next() {
		var finding domain.CandidateFindingRecord
		var evidenceJSON, createdAt string
		if err := rows.Scan(
			&finding.ID, &finding.RunID, &finding.UnitID, &finding.TraceID, &finding.Detector,
			&finding.Category, &finding.Severity, &finding.File, &finding.Line, &finding.Title,
			&finding.Explanation, &evidenceJSON, &finding.Suggestion, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan candidate finding: %w", err)
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &finding.Evidence); err != nil {
			return nil, fmt.Errorf("parse candidate evidence: %w", err)
		}
		finding.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse candidate created_at: %w", err)
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidate findings: %w", err)
	}
	return findings, nil
}

// GetReviewTrace 按 ID 返回一条已脱敏的模型审查证据链。
func (s *Store) GetReviewTrace(ctx context.Context, traceID string) (domain.ReviewTrace, error) {
	var trace domain.ReviewTrace
	var rejectionsJSON, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, unit_id, call_id, detector, status, prompt, response,
		       rejections_json, error_message, created_at
		FROM review_traces WHERE id = ?`, traceID,
	).Scan(
		&trace.ID, &trace.RunID, &trace.UnitID, &trace.CallID, &trace.Detector, &trace.Status,
		&trace.Prompt, &trace.Response, &rejectionsJSON, &trace.ErrorMessage, &createdAt,
	)
	if err != nil {
		return domain.ReviewTrace{}, fmt.Errorf("get review trace: %w", err)
	}
	if err := json.Unmarshal([]byte(rejectionsJSON), &trace.Rejections); err != nil {
		return domain.ReviewTrace{}, fmt.Errorf("parse trace rejections: %w", err)
	}
	trace.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.ReviewTrace{}, fmt.Errorf("parse trace created_at: %w", err)
	}
	return trace, nil
}

// ReplaceVerifiedFindings 原子替换一次 Run 的全部验证结果。
// Verifier 是确定性的，因此恢复时可以安全重算；事务保证不会留下半批结果。
func (s *Store) ReplaceVerifiedFindings(ctx context.Context, runID string, findings []domain.VerifiedFinding) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace verified findings: %w", err)
	}
	defer tx.Rollback()
	var status domain.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, runID).Scan(&status); err != nil {
		return fmt.Errorf("read verified finding run: %w", err)
	}
	if status != domain.RunStatusAggregating {
		return ErrRunNotAggregating
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM verified_findings WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete verified findings: %w", err)
	}
	for _, finding := range findings {
		if finding.RunID != runID {
			return fmt.Errorf("verified finding %q belongs to another run", finding.ID)
		}
		evidenceJSON, err := json.Marshal(finding.Evidence)
		if err != nil {
			return fmt.Errorf("marshal verified evidence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO verified_findings (
				id, run_id, candidate_id, trace_id, fingerprint, confidence,
				verification_source, verification_reason, category, severity, file_path,
				line, title, explanation, evidence_json, suggestion, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			finding.ID, finding.RunID, finding.CandidateID, finding.TraceID, finding.Fingerprint,
			finding.Confidence, finding.VerificationSource, finding.VerificationReason,
			finding.Category, finding.Severity, finding.File, finding.Line, finding.Title,
			finding.Explanation, string(evidenceJSON), finding.Suggestion, timeText(finding.CreatedAt),
		); err != nil {
			return fmt.Errorf("insert verified finding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit verified findings: %w", err)
	}
	return nil
}

// ListVerifiedFindings 返回已分类、去重并可用于报告的最终 Finding。
func (s *Store) ListVerifiedFindings(ctx context.Context, runID string) ([]domain.VerifiedFinding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, candidate_id, trace_id, fingerprint, confidence,
		       verification_source, verification_reason, category, severity, file_path,
		       line, title, explanation, evidence_json, suggestion, created_at
		FROM verified_findings WHERE run_id = ? ORDER BY file_path, line, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list verified findings: %w", err)
	}
	defer rows.Close()
	var findings []domain.VerifiedFinding
	for rows.Next() {
		var finding domain.VerifiedFinding
		var evidenceJSON, createdAt string
		if err := rows.Scan(
			&finding.ID, &finding.RunID, &finding.CandidateID, &finding.TraceID,
			&finding.Fingerprint, &finding.Confidence, &finding.VerificationSource,
			&finding.VerificationReason, &finding.Category, &finding.Severity,
			&finding.File, &finding.Line, &finding.Title, &finding.Explanation,
			&evidenceJSON, &finding.Suggestion, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan verified finding: %w", err)
		}
		if err := json.Unmarshal([]byte(evidenceJSON), &finding.Evidence); err != nil {
			return nil, fmt.Errorf("parse verified evidence: %w", err)
		}
		finding.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse verified finding created_at: %w", err)
		}
		findings = append(findings, finding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verified findings: %w", err)
	}
	return findings, nil
}

// SaveReport 原子保存权威 Markdown，并将 Run 推进到 reported。
// reported 状态允许重复写入相同产物，支持文件丢失后的恢复。
func (s *Store) SaveReport(ctx context.Context, report domain.Report, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save report: %w", err)
	}
	defer tx.Rollback()
	var status domain.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, report.RunID).Scan(&status); err != nil {
		return fmt.Errorf("read report run: %w", err)
	}
	if status != domain.RunStatusAggregating && status != domain.RunStatusReported {
		return ErrRunNotReportable
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO reports (run_id, output_path, content, content_sha256, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
		  output_path = excluded.output_path,
		  content = excluded.content,
		  content_sha256 = excluded.content_sha256,
		  created_at = excluded.created_at`,
		report.RunID, report.OutputPath, report.Content, report.ContentSHA256, timeText(report.CreatedAt),
	); err != nil {
		return fmt.Errorf("upsert report: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		domain.RunStatusReported, timeText(now), report.RunID,
		domain.RunStatusAggregating, domain.RunStatusReported,
	)
	if err != nil {
		return fmt.Errorf("mark run reported: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read reported result: %w", err)
	}
	if changed != 1 {
		return ErrRunNotReportable
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit report: %w", err)
	}
	return nil
}

// GetReport 返回 Run 已持久化的权威 Markdown 产物。
func (s *Store) GetReport(ctx context.Context, runID string) (domain.Report, error) {
	var report domain.Report
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT run_id, output_path, content, content_sha256, created_at
		FROM reports WHERE run_id = ?`, runID,
	).Scan(&report.RunID, &report.OutputPath, &report.Content, &report.ContentSHA256, &createdAt)
	if err != nil {
		return domain.Report{}, fmt.Errorf("get report: %w", err)
	}
	report.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.Report{}, fmt.Errorf("parse report created_at: %w", err)
	}
	return report, nil
}

// SaveSnapshot 为 Run 保存一次不可变的变更快照。
// 数据库主键保证同一个 Run 无法替换已保存的 Snapshot。
func (s *Store) SaveSnapshot(ctx context.Context, runID string, snapshot domain.ChangeSnapshot) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO change_snapshots (run_id, base_sha, head_sha, diff, diff_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		runID, snapshot.BaseSHA, snapshot.HeadSHA, snapshot.Diff, snapshot.DiffSHA256, timeText(snapshot.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert change snapshot: %w", err)
	}
	return nil
}

// GetSnapshot 读取 Run 固定下来的变更快照。
func (s *Store) GetSnapshot(ctx context.Context, runID string) (domain.ChangeSnapshot, error) {
	var snapshot domain.ChangeSnapshot
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT base_sha, head_sha, diff, diff_sha256, created_at
		FROM change_snapshots WHERE run_id = ?`, runID,
	).Scan(&snapshot.BaseSHA, &snapshot.HeadSHA, &snapshot.Diff, &snapshot.DiffSHA256, &createdAt)
	if err != nil {
		return domain.ChangeSnapshot{}, fmt.Errorf("get change snapshot: %w", err)
	}
	snapshot.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.ChangeSnapshot{}, fmt.Errorf("parse change snapshot created_at: %w", err)
	}
	return snapshot, nil
}

// SavePlan 原子保存 Planner 产出的 Unit，并将 Run 推进到 planned。
func (s *Store) SavePlan(ctx context.Context, runID string, units []domain.ReviewUnit, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save plan: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		domain.RunStatusPlanned, timeText(now), runID, domain.RunStatusFetched,
	)
	if err != nil {
		return fmt.Errorf("update run plan status: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read plan status update: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("run %q is not in fetched status", runID)
	}

	for _, unit := range units {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO review_units (
				id, run_id, unit_key, file_path, start_line, end_line, diff_hunk,
				risk, status, attempt, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			unit.ID, unit.RunID, unit.UnitKey, unit.FilePath, unit.StartLine, unit.EndLine, unit.DiffHunk, unit.Risk, unit.Status,
			unit.Attempt, timeText(unit.CreatedAt), timeText(unit.UpdatedAt),
		); err != nil {
			return fmt.Errorf("insert planned unit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save plan: %w", err)
	}
	return nil
}

// ReserveBudget 用单条写语句原子检查 Run 上限并创建费用预留。
func (s *Store) ReserveBudget(ctx context.Context, reservation budget.Reservation) error {
	result, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO budget_ledger (
			id, run_id, unit_id, tier, status, reserved_micros, created_at
		)
		SELECT ?, ?, ?, ?, ?, ?, ?
		WHERE ? + COALESCE((
			SELECT SUM(CASE status WHEN 'reserved' THEN reserved_micros WHEN 'settled' THEN actual_micros ELSE 0 END)
			FROM budget_ledger WHERE run_id = ?
		), 0) <= COALESCE((SELECT budget_limit_micros FROM runs WHERE id = ?), -1)`,
		reservation.ID, reservation.RunID, reservation.UnitID, reservation.Tier, budget.StatusReserved,
		reservation.ReservedMicros, timeText(reservation.CreatedAt), reservation.ReservedMicros,
		reservation.RunID, reservation.RunID,
	)
	if err != nil {
		return fmt.Errorf("insert budget reservation: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read reservation result: %w", err)
	}
	if inserted == 1 {
		return nil
	}

	var existing budget.Reservation
	var existingStatus string
	err = s.db.QueryRowContext(ctx, `
		SELECT id, run_id, unit_id, tier, reserved_micros, status
		FROM budget_ledger WHERE id = ?`, reservation.ID,
	).Scan(&existing.ID, &existing.RunID, &existing.UnitID, &existing.Tier, &existing.ReservedMicros, &existingStatus)
	if err == nil && existingStatus == budget.StatusReserved && existing.ID == reservation.ID && existing.RunID == reservation.RunID && existing.UnitID == reservation.UnitID && existing.Tier == reservation.Tier && existing.ReservedMicros == reservation.ReservedMicros {
		return nil
	}
	if err == nil {
		return budget.ErrReservationConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing reservation: %w", err)
	}
	return budget.ErrLimitExceeded
}

func (s *Store) SettleBudget(ctx context.Context, reservationID string, usage budget.Usage) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE budget_ledger
		SET status = ?, actual_micros = ?, input_tokens = ?, output_tokens = ?
		WHERE id = ? AND status = ? AND ? <= reserved_micros`,
		budget.StatusSettled, usage.ActualMicros, usage.InputTokens, usage.OutputTokens,
		reservationID, budget.StatusReserved, usage.ActualMicros,
	)
	if err != nil {
		return fmt.Errorf("settle budget reservation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read settlement result: %w", err)
	}
	if changed != 1 {
		var status string
		var actualMicros, inputTokens, outputTokens int64
		err := s.db.QueryRowContext(ctx, `
			SELECT status, actual_micros, input_tokens, output_tokens
			FROM budget_ledger WHERE id = ?`, reservationID,
		).Scan(&status, &actualMicros, &inputTokens, &outputTokens)
		if err == nil && status == budget.StatusSettled && actualMicros == usage.ActualMicros && inputTokens == usage.InputTokens && outputTokens == usage.OutputTokens {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read settled reservation: %w", err)
		}
		return fmt.Errorf("reservation %q cannot be settled", reservationID)
	}
	return nil
}

func (s *Store) ReleaseBudget(ctx context.Context, reservationID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE budget_ledger SET status = ? WHERE id = ? AND status = ?`,
		budget.StatusReleased, reservationID, budget.StatusReserved,
	)
	if err != nil {
		return fmt.Errorf("release budget reservation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read release result: %w", err)
	}
	if changed != 1 {
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM budget_ledger WHERE id = ?`, reservationID).Scan(&status)
		if err == nil && status == budget.StatusReleased {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read released reservation: %w", err)
		}
		return fmt.Errorf("reservation %q cannot be released", reservationID)
	}
	return nil
}

func (s *Store) BudgetSummary(ctx context.Context, runID string) (budget.Summary, error) {
	var summary budget.Summary
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'reserved' THEN reserved_micros ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'settled' THEN actual_micros ELSE 0 END), 0),
			COALESCE(SUM(CASE status WHEN 'reserved' THEN reserved_micros WHEN 'settled' THEN actual_micros ELSE 0 END), 0)
		FROM budget_ledger WHERE run_id = ?`, runID,
	).Scan(&summary.ReservedMicros, &summary.ActualMicros, &summary.CommittedMicros)
	if err != nil {
		return budget.Summary{}, fmt.Errorf("summarize budget: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT tier, COUNT(*), COALESCE(SUM(actual_micros), 0)
		FROM budget_ledger
		WHERE run_id = ? AND status = ?
		GROUP BY tier
		ORDER BY tier`, runID, budget.StatusSettled)
	if err != nil {
		return budget.Summary{}, fmt.Errorf("summarize budget tiers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tier budget.TierSummary
		if err := rows.Scan(&tier.Name, &tier.SettledCalls, &tier.ActualMicros); err != nil {
			return budget.Summary{}, fmt.Errorf("scan budget tier summary: %w", err)
		}
		summary.Tiers = append(summary.Tiers, tier)
	}
	if err := rows.Err(); err != nil {
		return budget.Summary{}, fmt.Errorf("iterate budget tier summary: %w", err)
	}
	return summary, nil
}

// ClaimRun 获取或续期 Run 的 lease；其他 owner 只能在 lease 过期后接管。
func (s *Store) ClaimRun(ctx context.Context, runID, owner string, now time.Time, ttl time.Duration) (domain.Run, error) {
	if strings.TrimSpace(owner) == "" {
		return domain.Run{}, ErrLeaseOwnerRequired
	}
	expiresAt := now.Add(ttl)
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs
		SET lease_owner = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ?
		  AND (lease_owner IS NULL OR lease_expires_at <= ? OR lease_owner = ?)`,
		owner, timeText(expiresAt), timeText(now), runID, timeText(now), owner,
	)
	if err != nil {
		return domain.Run{}, fmt.Errorf("claim run: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return domain.Run{}, fmt.Errorf("claim result: %w", err)
	}
	if updated == 0 {
		return domain.Run{}, ErrLeaseHeld
	}
	return s.GetRun(ctx, runID)
}

// ReleaseRunLease 只允许当前 owner 主动释放 lease，避免旧进程清掉新 owner 的 lease。
func (s *Store) ReleaseRunLease(ctx context.Context, runID, owner string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET lease_owner = NULL, lease_expires_at = NULL
		WHERE id = ? AND lease_owner = ?`, runID, owner)
	if err != nil {
		return fmt.Errorf("release run lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read release run lease result: %w", err)
	}
	if changed != 1 {
		return ErrLeaseHeld
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(row rowScanner) (domain.Run, error) {
	var run domain.Run
	var leaseExpiresAt sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(
		&run.ID, &run.SourceURL, &run.Provider, &run.Repository, &run.ChangeNumber,
		&run.Status, &run.BudgetLimitMicros, &run.LeaseOwner, &leaseExpiresAt,
		&createdAt, &updatedAt,
	); err != nil {
		return domain.Run{}, fmt.Errorf("get run: %w", err)
	}
	var err error
	run.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.Run{}, fmt.Errorf("parse run created_at: %w", err)
	}
	run.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return domain.Run{}, fmt.Errorf("parse run updated_at: %w", err)
	}
	if leaseExpiresAt.Valid {
		run.LeaseExpiresAt, err = parseTime(leaseExpiresAt.String)
		if err != nil {
			return domain.Run{}, fmt.Errorf("parse run lease_expires_at: %w", err)
		}
	}
	return run, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return timeText(value)
}

func timeText(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
