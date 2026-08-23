// Package tools 提供声明式工具注册、权限校验和受限执行能力。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidConfig     = errors.New("invalid tool configuration")
	ErrToolNotRegistered = errors.New("tool is not registered")
	ErrPermissionDenied  = errors.New("tool permission denied")
	ErrInvalidCall       = errors.New("invalid tool call")
	ErrResultTooLarge    = errors.New("tool result exceeds configured limit")
)

// Registration 是配置文件中允许暴露给 Reviewer 的工具声明。
type Registration struct {
	Name           string   `yaml:"name"`
	Description    string   `yaml:"description"`
	Implementation string   `yaml:"implementation"`
	Permissions    []string `yaml:"permissions"`
	MaxResultBytes int      `yaml:"max_result_bytes"`
}

// Definition 是发送给模型的工具名称、说明和参数 Schema。
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Call 是模型提出的一次结构化工具调用。
type Call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Result 是可以作为 Observation 返回给模型的工具结果。
type Result struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Tool 是工具实现的最小接口。工具名称由 YAML 声明，不由实现决定。
type Tool interface {
	RequiredPermission() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, arguments json.RawMessage) (string, error)
}

type registeredTool struct {
	registration Registration
	tool         Tool
}

// Registry 只保存通过配置、实现和权限三重校验的工具。
type Registry struct {
	ordered []Definition
	tools   map[string]registeredTool
}

// NewRegistry 将声明映射到维护者提供的实现；模型不能注册或替换实现。
func NewRegistry(registrations []Registration, implementations map[string]Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]registeredTool, len(registrations))}
	for _, registration := range registrations {
		registration.Name = strings.TrimSpace(registration.Name)
		registration.Description = strings.TrimSpace(registration.Description)
		registration.Implementation = strings.TrimSpace(registration.Implementation)
		if registration.Name == "" || registration.Description == "" || registration.Implementation == "" || registration.MaxResultBytes <= 0 {
			return nil, fmt.Errorf("%w: incomplete registration %q", ErrInvalidConfig, registration.Name)
		}
		if _, exists := registry.tools[registration.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate tool %q", ErrInvalidConfig, registration.Name)
		}
		implementation, ok := implementations[registration.Implementation]
		if !ok || implementation == nil {
			return nil, fmt.Errorf("%w: implementation %q is unavailable", ErrInvalidConfig, registration.Implementation)
		}
		required := strings.TrimSpace(implementation.RequiredPermission())
		if required == "" || !contains(registration.Permissions, required) {
			return nil, fmt.Errorf("%w: %s requires %s", ErrPermissionDenied, registration.Name, required)
		}
		parameters := implementation.Parameters()
		if len(parameters) == 0 || !json.Valid(parameters) {
			return nil, fmt.Errorf("%w: %s has invalid parameter schema", ErrInvalidConfig, registration.Name)
		}
		registry.tools[registration.Name] = registeredTool{registration: registration, tool: implementation}
		registry.ordered = append(registry.ordered, Definition{Name: registration.Name, Description: registration.Description, Parameters: append(json.RawMessage(nil), parameters...)})
	}
	return registry, nil
}

// Definitions 返回配置顺序稳定的工具定义，保证 Prompt 和测试可复现。
func (registry *Registry) Definitions() []Definition {
	if registry == nil {
		return nil
	}
	definitions := make([]Definition, len(registry.ordered))
	copy(definitions, registry.ordered)
	for index := range definitions {
		definitions[index].Parameters = append(json.RawMessage(nil), definitions[index].Parameters...)
	}
	return definitions
}

// Execute 在分发前拒绝未知工具和畸形参数，并限制返回内容大小。
func (registry *Registry) Execute(ctx context.Context, call Call) (Result, error) {
	if registry == nil || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" || len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return Result{}, ErrInvalidCall
	}
	registered, ok := registry.tools[call.Name]
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrToolNotRegistered, call.Name)
	}
	content, err := registered.tool.Execute(ctx, call.Arguments)
	if err != nil {
		return Result{}, fmt.Errorf("execute tool %s: %w", call.Name, err)
	}
	if len([]byte(content)) > registered.registration.MaxResultBytes {
		return Result{}, fmt.Errorf("%w: %s", ErrResultTooLarge, call.Name)
	}
	return Result{CallID: call.ID, Name: call.Name, Content: content}, nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == expected {
			return true
		}
	}
	return false
}
