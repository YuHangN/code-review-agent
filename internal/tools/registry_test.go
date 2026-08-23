package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/tools"
)

func TestRegistryLoadsDeclarationsAndExecutesAuthorizedImplementation(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "tools.yaml")
	content := []byte("agent:\n  max_rounds: 4\n  max_tool_calls: 6\ntools:\n  - name: read_file\n    description: 读取固定版本文件\n    implementation: repository_file\n    permissions: [snapshot_read]\n    max_result_bytes: 4096\n")
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := tools.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(config.Tools, map[string]tools.Tool{
		"repository_file": fixedTool{result: `{"path":"internal/auth/token.go"}`},
	})
	if err != nil {
		t.Fatal(err)
	}

	definitions := registry.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "read_file" || definitions[0].Description != "读取固定版本文件" {
		t.Fatalf("definitions = %#v", definitions)
	}
	result, err := registry.Execute(context.Background(), tools.Call{
		ID: "tool-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"internal/auth/token.go"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CallID != "tool-1" || result.Name != "read_file" || result.Content != `{"path":"internal/auth/token.go"}` {
		t.Fatalf("result = %#v", result)
	}
}

func TestRegistryRejectsUndeclaredToolBeforeExecution(t *testing.T) {
	called := false
	registry, err := tools.NewRegistry(nil, map[string]tools.Tool{
		"repository_file": callbackTool{execute: func(json.RawMessage) { called = true }},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = registry.Execute(context.Background(), tools.Call{ID: "tool-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)})
	if !errors.Is(err, tools.ErrToolNotRegistered) {
		t.Fatalf("execute error = %v", err)
	}
	if called {
		t.Fatal("undeclared implementation was executed")
	}
}

func TestRegistryRejectsDeclarationWithoutImplementationPermission(t *testing.T) {
	_, err := tools.NewRegistry([]tools.Registration{{
		Name: "read_file", Description: "读取文件", Implementation: "repository_file", Permissions: []string{"network"}, MaxResultBytes: 4096,
	}}, map[string]tools.Tool{"repository_file": fixedTool{}})
	if !errors.Is(err, tools.ErrPermissionDenied) {
		t.Fatalf("registry error = %v", err)
	}
}

type fixedTool struct {
	result string
}

func (fixedTool) RequiredPermission() string {
	return "snapshot_read"
}

func (fixedTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}

func (tool fixedTool) Execute(context.Context, json.RawMessage) (string, error) {
	return tool.result, nil
}

type callbackTool struct {
	execute func(json.RawMessage)
}

func (callbackTool) RequiredPermission() string {
	return "snapshot_read"
}

func (callbackTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

func (tool callbackTool) Execute(_ context.Context, arguments json.RawMessage) (string, error) {
	tool.execute(arguments)
	return "", nil
}
