package workflow

import (
	"errors"
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
