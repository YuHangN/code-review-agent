package tools

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// AgentLimits 限制单个 Review Unit 的模型轮次和工具调用总数。
type AgentLimits struct {
	MaxRounds    int `yaml:"max_rounds"`
	MaxToolCalls int `yaml:"max_tool_calls"`
}

// Config 是 Tool Registry 和 Reviewer Agent Loop 的声明式配置。
type Config struct {
	Agent AgentLimits    `yaml:"agent"`
	Tools []Registration `yaml:"tools"`
}

// LoadConfig 读取并校验工具配置；无效配置在任何模型调用前失败。
func LoadConfig(path string) (Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read tools config: %w", err)
	}
	var config Config
	if err := yaml.Unmarshal(content, &config); err != nil {
		return Config{}, fmt.Errorf("parse tools config: %w", err)
	}
	if config.Agent.MaxRounds <= 0 || config.Agent.MaxToolCalls <= 0 {
		return Config{}, fmt.Errorf("%w: agent limits must be positive", ErrInvalidConfig)
	}
	seen := make(map[string]struct{}, len(config.Tools))
	for _, registration := range config.Tools {
		if registration.Name == "" || registration.Description == "" || registration.Implementation == "" || registration.MaxResultBytes <= 0 {
			return Config{}, fmt.Errorf("%w: incomplete tool registration", ErrInvalidConfig)
		}
		if _, exists := seen[registration.Name]; exists {
			return Config{}, fmt.Errorf("%w: duplicate tool %q", ErrInvalidConfig, registration.Name)
		}
		seen[registration.Name] = struct{}{}
	}
	return config, nil
}
