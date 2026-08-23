package checker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/checker"
)

func TestDockerRunnerUsesNetworkOnlyForDependencyPreparation(t *testing.T) {
	executor := &recordingExecutor{}
	runner, err := checker.NewDockerRunner(executor, checker.DockerSettings{
		Binary: "docker", Image: "checker@sha256:" + strings.Repeat("a", 64), CPUs: "1", Memory: "1g", TmpSize: "1g", PIDs: 128,
		DependencyTimeout: time.Minute, Proxy: "https://proxy.golang.org",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := runner.Prepare(context.Background(), "/source", "/cache"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), "/source", "/cache", checker.Definition{Name: "go_vet", Implementation: "go_vet", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 2 {
		t.Fatalf("calls = %#v", executor.calls)
	}
	prepare, lint := strings.Join(executor.calls[0], " "), strings.Join(executor.calls[1], " ")
	if !strings.Contains(prepare, "--network bridge") || !strings.Contains(prepare, "GOPROXY=https://proxy.golang.org") || !strings.Contains(prepare, "go mod download") {
		t.Fatalf("unsafe dependency command: %s", prepare)
	}
	for _, required := range []string{"--network none", "--read-only", "--cap-drop ALL", "no-new-privileges", "size=1g", "XDG_CACHE_HOME=/tmp/staticcheck", "/source:/workspace:ro", "/cache:/go/pkg/mod:ro", "go vet ./..."} {
		if !strings.Contains(lint, required) {
			t.Fatalf("lint command missing %q: %s", required, lint)
		}
	}
}

func TestDockerRunnerRejectsUnknownImplementation(t *testing.T) {
	runner, err := checker.NewDockerRunner(&recordingExecutor{}, checker.DockerSettings{Binary: "docker", Image: "checker@sha256:" + strings.Repeat("a", 64), CPUs: "1", Memory: "1g", TmpSize: "1g", PIDs: 128, DependencyTimeout: time.Minute, Proxy: "https://proxy.golang.org"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), "/source", "/cache", checker.Definition{Name: "evil", Implementation: "shell", Timeout: time.Second}); err == nil {
		t.Fatal("expected unknown implementation to be rejected")
	}
}

func TestResolvingDockerRunnerInspectsImageLazilyAndOnlyOnce(t *testing.T) {
	executor := &recordingExecutor{result: checker.CommandResult{Output: "sha256:" + strings.Repeat("b", 64)}}
	runner, err := checker.NewResolvingDockerRunner(executor, checker.DockerSettings{Binary: "docker", Image: "checker:local", CPUs: "1", Memory: "1g", TmpSize: "1g", PIDs: 128, DependencyTimeout: time.Minute, Proxy: "https://proxy.golang.org"})
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("constructor accessed Docker: %#v", executor.calls)
	}
	if _, err := runner.Prepare(context.Background(), "/source", "/cache"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), "/source", "/cache", checker.Definition{Name: "go_vet", Implementation: "go_vet", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 3 || !strings.Contains(strings.Join(executor.calls[0], " "), "image inspect") {
		t.Fatalf("calls = %#v", executor.calls)
	}
}

type recordingExecutor struct {
	calls  [][]string
	result checker.CommandResult
}

func (executor *recordingExecutor) Execute(_ context.Context, binary string, arguments []string) (checker.CommandResult, error) {
	executor.calls = append(executor.calls, append([]string{binary}, arguments...))
	return executor.result, nil
}
