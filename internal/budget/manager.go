// Package budget 管理模型调用的预算预留、结算和释放。
package budget

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrLimitExceeded       = errors.New("run budget limit exceeded")
	ErrReservationConflict = errors.New("budget reservation conflicts with existing record")
	ErrInvalidReservation  = errors.New("invalid budget reservation")
)

const (
	StatusReserved = "reserved"
	StatusSettled  = "settled"
	StatusReleased = "released"
)

// Reservation 是一次模型调用发起前锁定的最大费用。
type Reservation struct {
	ID             string
	RunID          string
	UnitID         string
	Tier           string
	ReservedMicros int64
	CreatedAt      time.Time
}

// Usage 是模型提供方返回的真实费用和 token 用量。
type Usage struct {
	ActualMicros int64
	InputTokens  int64
	OutputTokens int64
}

// Summary 汇总一个 Run 已结算和仍预留的费用。
type Summary struct {
	ReservedMicros  int64
	ActualMicros    int64
	CommittedMicros int64
}

// Store 是 Manager 所需的最小原子持久化能力。
type Store interface {
	ReserveBudget(ctx context.Context, reservation Reservation) error
	SettleBudget(ctx context.Context, reservationID string, usage Usage) error
	ReleaseBudget(ctx context.Context, reservationID string) error
	BudgetSummary(ctx context.Context, runID string) (Summary, error)
}

type Manager struct{ store Store }

func NewManager(store Store) Manager { return Manager{store: store} }

func (m Manager) Reserve(ctx context.Context, reservation Reservation) error {
	if reservation.ID == "" || reservation.RunID == "" || reservation.UnitID == "" || reservation.Tier == "" || reservation.ReservedMicros <= 0 || reservation.CreatedAt.IsZero() {
		return ErrInvalidReservation
	}
	if err := m.store.ReserveBudget(ctx, reservation); err != nil {
		return fmt.Errorf("reserve budget: %w", err)
	}
	return nil
}

func (m Manager) Settle(ctx context.Context, reservationID string, usage Usage) error {
	if reservationID == "" || usage.ActualMicros < 0 || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return ErrInvalidReservation
	}
	if err := m.store.SettleBudget(ctx, reservationID, usage); err != nil {
		return fmt.Errorf("settle budget: %w", err)
	}
	return nil
}

func (m Manager) Release(ctx context.Context, reservationID string) error {
	if reservationID == "" {
		return ErrInvalidReservation
	}
	if err := m.store.ReleaseBudget(ctx, reservationID); err != nil {
		return fmt.Errorf("release budget: %w", err)
	}
	return nil
}

func (m Manager) Summary(ctx context.Context, runID string) (Summary, error) {
	if runID == "" {
		return Summary{}, ErrInvalidReservation
	}
	return m.store.BudgetSummary(ctx, runID)
}
