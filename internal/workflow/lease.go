package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidLeaseSettings = errors.New("invalid lease settings")

// LeaseSettings 定义 lease 的有效期及续期频率。
type LeaseSettings struct {
	TTL           time.Duration
	RenewInterval time.Duration
}

// Validate 确保 lease 不会在下一次续期前过期。
func (s LeaseSettings) Validate() error {
	if s.TTL <= 0 || s.RenewInterval <= 0 || s.RenewInterval >= s.TTL {
		return ErrInvalidLeaseSettings
	}
	return nil
}

// MaintainLease 在 context 存活期间定期续期，并在续期失败时报告错误。
// 调用方需要先通过 Resume 成功领取 lease，再启动这个后台循环。
func (s Service) MaintainLease(ctx context.Context, runID, owner string, settings LeaseSettings) (<-chan error, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}

	errors := make(chan error, 1)
	go func() {
		defer close(errors)
		ticker := time.NewTicker(settings.RenewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := s.store.ClaimRun(ctx, runID, owner, now.UTC(), settings.TTL); err != nil {
					errors <- fmt.Errorf("renew run lease: %w", err)
					return
				}
			}
		}
	}()
	return errors, nil
}
