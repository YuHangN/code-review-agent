// Package config 负责读取本地运行时配置。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/YuHangN/code-review-agent/internal/llm"
	"gopkg.in/yaml.v3"
)

// Runtime 是影响本地执行行为的配置，不包含 Review 策略本身。
type Runtime struct {
	LeaseTTL           time.Duration
	LeaseRenewInterval time.Duration
	SQLiteBusyTimeout  time.Duration
	DefaultBudgetCents int64
	Currency           string
	MaxFindingsPerUnit int
	LLMRequestTimeout  time.Duration
	DefaultLLMTier     string
	LLMTiers           map[string]llm.Tier
}

type rawLLMTier struct {
	Provider                          string `yaml:"provider"`
	Model                             string `yaml:"model"`
	InputPriceMicrosPerMillionTokens  int64  `yaml:"input_price_micros_per_million_tokens"`
	OutputPriceMicrosPerMillionTokens int64  `yaml:"output_price_micros_per_million_tokens"`
	MaxOutputTokens                   int64  `yaml:"max_output_tokens"`
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
			MaxFindingsPerUnit int    `yaml:"max_findings_per_unit"`
		} `yaml:"review"`
		LLM struct {
			RequestTimeout string                `yaml:"request_timeout"`
			DefaultTier    string                `yaml:"default_tier"`
			Tiers          map[string]rawLLMTier `yaml:"tiers"`
		} `yaml:"llm"`
	}
	if err := yaml.Unmarshal(content, &raw); err != nil {
		return Runtime{}, fmt.Errorf("parse runtime config: %w", err)
	}

	tiers := make(map[string]llm.Tier, len(raw.LLM.Tiers))
	for name, tier := range raw.LLM.Tiers {
		tiers[name] = llm.Tier{
			Provider:                    tier.Provider,
			Model:                       tier.Model,
			InputPriceMicrosPerMillion:  tier.InputPriceMicrosPerMillionTokens,
			OutputPriceMicrosPerMillion: tier.OutputPriceMicrosPerMillionTokens,
			MaxOutputTokens:             tier.MaxOutputTokens,
		}
	}
	runtime, err := parseRuntime(raw.Runtime.LeaseTTL, raw.Runtime.LeaseRenewInterval, raw.Runtime.SQLiteBusyTimeout, raw.Review.DefaultBudgetCents, raw.Review.Currency, raw.Review.MaxFindingsPerUnit, raw.LLM.RequestTimeout, raw.LLM.DefaultTier, tiers)
	if err != nil {
		return Runtime{}, fmt.Errorf("validate runtime config: %w", err)
	}
	return runtime, nil
}

func parseRuntime(leaseTTL, leaseRenewInterval, sqliteBusyTimeout string, defaultBudgetCents int64, currency string, maxFindingsPerUnit int, llmRequestTimeout, defaultLLMTier string, tiers map[string]llm.Tier) (Runtime, error) {
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
	if maxFindingsPerUnit <= 0 {
		return Runtime{}, fmt.Errorf("max_findings_per_unit must be positive")
	}
	requestTimeout, err := time.ParseDuration(llmRequestTimeout)
	if err != nil || requestTimeout <= 0 {
		return Runtime{}, fmt.Errorf("llm.request_timeout must be a positive duration")
	}
	defaultLLMTier = strings.TrimSpace(defaultLLMTier)
	if defaultLLMTier == "" {
		return Runtime{}, fmt.Errorf("llm.default_tier must not be empty")
	}
	if _, ok := tiers[defaultLLMTier]; !ok {
		return Runtime{}, fmt.Errorf("llm.default_tier %q is not configured", defaultLLMTier)
	}
	for name, tier := range tiers {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(tier.Provider) == "" || strings.TrimSpace(tier.Model) == "" || tier.InputPriceMicrosPerMillion <= 0 || tier.OutputPriceMicrosPerMillion <= 0 || tier.MaxOutputTokens <= 0 {
			return Runtime{}, fmt.Errorf("llm tier %q is invalid", name)
		}
	}

	return Runtime{
		LeaseTTL:           ttl,
		LeaseRenewInterval: renewInterval,
		SQLiteBusyTimeout:  busyTimeout,
		DefaultBudgetCents: defaultBudgetCents,
		Currency:           currency,
		MaxFindingsPerUnit: maxFindingsPerUnit,
		LLMRequestTimeout:  requestTimeout,
		DefaultLLMTier:     defaultLLMTier,
		LLMTiers:           tiers,
	}, nil
}
