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
	LLMFallbackOrder   []string
	LLMTiers           map[string]llm.Tier
	Checkers           CheckerConfig
}

type CheckerDefinition struct {
	Name, Implementation string
	Timeout              time.Duration
}
type CheckerConfig struct {
	Enabled                                           bool
	DockerBinary, Image, CPUs, Memory, TmpSize, Proxy string
	PIDs                                              int
	DependencyTimeout, ImageInspectRetryDelay         time.Duration
	ImageInspectAttempts                              int
	Definitions                                       []CheckerDefinition
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
			FallbackOrder  []string              `yaml:"fallback_order"`
			Tiers          map[string]rawLLMTier `yaml:"tiers"`
		} `yaml:"llm"`
		Checkers struct {
			Enabled                bool   `yaml:"enabled"`
			DockerBinary           string `yaml:"docker_binary"`
			Image                  string `yaml:"image"`
			ImageInspectAttempts   int    `yaml:"image_inspect_attempts"`
			ImageInspectRetryDelay string `yaml:"image_inspect_retry_delay"`
			CPUs                   string `yaml:"cpus"`
			Memory                 string `yaml:"memory"`
			TmpSize                string `yaml:"tmp_size"`
			PIDs                   int    `yaml:"pids"`
			DependencyTimeout      string `yaml:"dependency_timeout"`
			Proxy                  string `yaml:"proxy"`
			Definitions            []struct {
				Name           string `yaml:"name"`
				Implementation string `yaml:"implementation"`
				Timeout        string `yaml:"timeout"`
			} `yaml:"definitions"`
		} `yaml:"checkers"`
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
	runtime, err := parseRuntime(raw.Runtime.LeaseTTL, raw.Runtime.LeaseRenewInterval, raw.Runtime.SQLiteBusyTimeout, raw.Review.DefaultBudgetCents, raw.Review.Currency, raw.Review.MaxFindingsPerUnit, raw.LLM.RequestTimeout, raw.LLM.DefaultTier, raw.LLM.FallbackOrder, tiers)
	if err != nil {
		return Runtime{}, fmt.Errorf("validate runtime config: %w", err)
	}
	if raw.Checkers.Enabled {
		if raw.Checkers.ImageInspectAttempts == 0 {
			raw.Checkers.ImageInspectAttempts = 3
		}
		if raw.Checkers.ImageInspectRetryDelay == "" {
			raw.Checkers.ImageInspectRetryDelay = "500ms"
		}
		dependencyTimeout, parseErr := time.ParseDuration(raw.Checkers.DependencyTimeout)
		imageInspectRetryDelay, retryParseErr := time.ParseDuration(raw.Checkers.ImageInspectRetryDelay)
		if parseErr != nil || dependencyTimeout <= 0 || retryParseErr != nil || imageInspectRetryDelay <= 0 || raw.Checkers.ImageInspectAttempts <= 0 || raw.Checkers.DockerBinary == "" || raw.Checkers.Image == "" || raw.Checkers.CPUs == "" || raw.Checkers.Memory == "" || raw.Checkers.TmpSize == "" || raw.Checkers.PIDs <= 0 || !strings.HasPrefix(raw.Checkers.Proxy, "https://") || len(raw.Checkers.Definitions) == 0 {
			return Runtime{}, fmt.Errorf("validate checker config")
		}
		runtime.Checkers = CheckerConfig{Enabled: true, DockerBinary: raw.Checkers.DockerBinary, Image: raw.Checkers.Image, ImageInspectAttempts: raw.Checkers.ImageInspectAttempts, ImageInspectRetryDelay: imageInspectRetryDelay, CPUs: raw.Checkers.CPUs, Memory: raw.Checkers.Memory, TmpSize: raw.Checkers.TmpSize, PIDs: raw.Checkers.PIDs, DependencyTimeout: dependencyTimeout, Proxy: raw.Checkers.Proxy}
		for _, definition := range raw.Checkers.Definitions {
			timeout, parseErr := time.ParseDuration(definition.Timeout)
			if parseErr != nil || timeout <= 0 || definition.Name == "" || (definition.Implementation != "go_vet" && definition.Implementation != "staticcheck") {
				return Runtime{}, fmt.Errorf("validate checker definition")
			}
			runtime.Checkers.Definitions = append(runtime.Checkers.Definitions, CheckerDefinition{Name: definition.Name, Implementation: definition.Implementation, Timeout: timeout})
		}
	}
	return runtime, nil
}

func parseRuntime(leaseTTL, leaseRenewInterval, sqliteBusyTimeout string, defaultBudgetCents int64, currency string, maxFindingsPerUnit int, llmRequestTimeout, defaultLLMTier string, fallbackOrder []string, tiers map[string]llm.Tier) (Runtime, error) {
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
	if len(fallbackOrder) == 0 {
		fallbackOrder = []string{defaultLLMTier}
	}
	if fallbackOrder[0] != defaultLLMTier {
		return Runtime{}, fmt.Errorf("llm.fallback_order must start with default_tier %q", defaultLLMTier)
	}
	seenFallbackTiers := make(map[string]struct{}, len(fallbackOrder))
	for _, name := range fallbackOrder {
		if _, ok := tiers[name]; !ok {
			return Runtime{}, fmt.Errorf("llm.fallback_order tier %q is not configured", name)
		}
		if _, exists := seenFallbackTiers[name]; exists {
			return Runtime{}, fmt.Errorf("llm.fallback_order contains duplicate tier %q", name)
		}
		seenFallbackTiers[name] = struct{}{}
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
		LLMFallbackOrder:   append([]string(nil), fallbackOrder...),
		LLMTiers:           tiers,
	}, nil
}
