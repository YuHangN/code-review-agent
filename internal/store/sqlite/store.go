package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/migrations"
	_ "modernc.org/sqlite"
)

var ErrLeaseHeld = errors.New("run lease is held by another owner")

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	store := &Store{db: db}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure database: %w", err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("begin migration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, migrations.SQL); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		return nil, fmt.Errorf("run migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("commit migration: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) CreateRun(ctx context.Context, run domain.Run, units []domain.ReviewUnit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create run: %w", err)
	}
	defer tx.Rollback()

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
				id, run_id, unit_key, file_path, risk, status, attempt, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			unit.ID, unit.RunID, unit.UnitKey, unit.FilePath, unit.Risk, unit.Status,
			unit.Attempt, timeText(unit.CreatedAt), timeText(unit.UpdatedAt),
		); err != nil {
			return fmt.Errorf("insert review unit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create run: %w", err)
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source_url, provider, repository, change_number, status, budget_limit_micros,
		       COALESCE(lease_owner, ''), lease_expires_at, created_at, updated_at
		FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

func (s *Store) ListUnits(ctx context.Context, runID string) ([]domain.ReviewUnit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, unit_key, file_path, risk, status, attempt, created_at, updated_at
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
			&unit.ID, &unit.RunID, &unit.UnitKey, &unit.FilePath, &unit.Risk,
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

func (s *Store) ClaimRun(ctx context.Context, runID, owner string, now time.Time, ttl time.Duration) (domain.Run, error) {
	expiresAt := now.Add(ttl)
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs
		SET lease_owner = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ?
		  AND (lease_owner IS NULL OR lease_owner = '' OR lease_expires_at IS NULL OR lease_expires_at <= ? OR lease_owner = ?)`,
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
