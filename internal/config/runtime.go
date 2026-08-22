// Package config 负责读取本地运行时配置。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Runtime 是影响本地执行行为的配置，不包含 Review 策略本身。
type Runtime struct {
	LeaseTTL           time.Duration
	LeaseRenewInterval time.Duration
	SQLiteBusyTimeout  time.Duration
	DefaultBudgetCents int64
	Currency           string
}

// LoadRuntime 从 YAML 文件读取并校验运行时配置。
func LoadRuntime(path string) (Runtime, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Runtime{}, fmt.Errorf("read runtime config: %w", err)
	}

	var raw struct {
		Runtime struct {
			LeaseTTL           string `yaml:"lease_ttl"`
			LeaseRenewInterval string `yaml:"lease_renew_interval"`
			SQLiteBusyTimeout  string `yaml:"sqlite_busy_timeout"`
		} `yaml:"runtime"`
		Review struct {
			DefaultBudgetCents int64  `yaml:"default_budget_cents"`
			Currency           string `yaml:"currency"`
		} `yaml:"review"`
	}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		return Runtime{}, fmt.Errorf("parse runtime config: %w", err)
	}

	runtime, err := parseRuntime(raw.Runtime.LeaseTTL, raw.Runtime.LeaseRenewInterval, raw.Runtime.SQLiteBusyTimeout, raw.Review.DefaultBudgetCents, raw.Review.Currency)
	if err != nil {
		return Runtime{}, fmt.Errorf("validate runtime config: %w", err)
	}
	return runtime, nil
}

func parseRuntime(leaseTTL, leaseRenewInterval, sqliteBusyTimeout string, defaultBudgetCents int64, currency string) (Runtime, error) {
	ttl, err := time.ParseDuration(leaseTTL)
	if err != nil || ttl <= 0 {
		return Runtime{}, fmt.Errorf("lease_ttl must be a positive duration")
	}
	renewInterval, err := time.ParseDuration(leaseRenewInterval)
	if err != nil || renewInterval <= 0 || renewInterval >= ttl {
		return Runtime{}, fmt.Errorf("lease_renew_interval must be positive and smaller than lease_ttl")
	}
	busyTimeout, err := time.ParseDuration(sqliteBusyTimeout)
	if err != nil || busyTimeout <= 0 {
		return Runtime{}, fmt.Errorf("sqlite_busy_timeout must be a positive duration")
	}
	if defaultBudgetCents <= 0 {
		return Runtime{}, fmt.Errorf("default_budget_cents must be positive")
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency != "USD" {
		return Runtime{}, fmt.Errorf("currency must be USD")
	}

	return Runtime{
		LeaseTTL:           ttl,
		LeaseRenewInterval: renewInterval,
		SQLiteBusyTimeout:  busyTimeout,
		DefaultBudgetCents: defaultBudgetCents,
		Currency:           currency,
	}, nil
}
