package checker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var immutableImagePattern = regexp.MustCompile(`(?:@sha256:|^sha256:)[a-f0-9]{64}$`)

type Definition struct {
	Name           string
	Implementation string
	Timeout        time.Duration
}

type DockerSettings struct {
	Binary                 string
	Image                  string
	ImageInspectAttempts   int
	ImageInspectRetryDelay time.Duration
	CPUs                   string
	Memory                 string
	TmpSize                string
	PIDs                   int
	DependencyTimeout      time.Duration
	Proxy                  string
}

type CommandResult struct {
	Output   string
	ExitCode int
}

type CommandExecutor interface {
	Execute(ctx context.Context, binary string, arguments []string) (CommandResult, error)
}

type DockerRunner struct {
	executor CommandExecutor
	settings DockerSettings
}

// ResolvingDockerRunner 延迟到 checking 阶段才把 tag 解析成不可变 image ID。
type ResolvingDockerRunner struct {
	executor CommandExecutor
	settings DockerSettings
	resolved *DockerRunner
}

func NewResolvingDockerRunner(executor CommandExecutor, settings DockerSettings) (*ResolvingDockerRunner, error) {
	if executor == nil || settings.Binary == "" || settings.Image == "" || settings.ImageInspectAttempts <= 0 || settings.ImageInspectRetryDelay <= 0 || settings.CPUs == "" || settings.Memory == "" || settings.TmpSize == "" || settings.PIDs <= 0 || settings.DependencyTimeout <= 0 || !strings.HasPrefix(settings.Proxy, "https://") {
		return nil, fmt.Errorf("invalid resolving docker checker settings")
	}
	return &ResolvingDockerRunner{executor: executor, settings: settings}, nil
}

func (runner *ResolvingDockerRunner) Prepare(ctx context.Context, sourceDir, cacheDir string) (CommandResult, error) {
	resolved, err := runner.resolve(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	return resolved.Prepare(ctx, sourceDir, cacheDir)
}

func (runner *ResolvingDockerRunner) Run(ctx context.Context, sourceDir, cacheDir string, definition Definition) (CommandResult, error) {
	resolved, err := runner.resolve(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	return resolved.Run(ctx, sourceDir, cacheDir, definition)
}

func (runner *ResolvingDockerRunner) resolve(ctx context.Context) (*DockerRunner, error) {
	if runner.resolved != nil {
		return runner.resolved, nil
	}
	image, err := resolveImageWithRetry(ctx, runner.executor, runner.settings.Binary, runner.settings.Image, runner.settings.ImageInspectAttempts, runner.settings.ImageInspectRetryDelay)
	if err != nil {
		return nil, err
	}
	settings := runner.settings
	settings.Image = image
	resolved, err := NewDockerRunner(runner.executor, settings)
	if err != nil {
		return nil, err
	}
	runner.resolved = &resolved
	return runner.resolved, nil
}

// ResolveImage 将维护者配置的本地 tag 解析为本机不可变 image ID。
func ResolveImage(ctx context.Context, executor CommandExecutor, binary, image string) (string, error) {
	if executor == nil || binary == "" || image == "" {
		return "", fmt.Errorf("invalid image resolver")
	}
	result, err := executor.Execute(ctx, binary, []string{"image", "inspect", "--format={{.Id}}", image})
	if err != nil {
		return "", err
	}
	resolved := strings.TrimSpace(result.Output)
	if result.ExitCode != 0 || !immutableImagePattern.MatchString(resolved) {
		if resolved == "" {
			resolved = "Docker returned no image ID"
		}
		return "", fmt.Errorf("checker image %q is unavailable or not immutable: %s; run `make checker-image` from the review-agent source directory to build it", image, resolved)
	}
	return resolved, nil
}

func resolveImageWithRetry(ctx context.Context, executor CommandExecutor, binary, image string, attempts int, delay time.Duration) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		resolved, err := ResolveImage(ctx, executor, binary, image)
		if err == nil {
			return resolved, nil
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", lastErr
}

func NewDockerRunner(executor CommandExecutor, settings DockerSettings) (DockerRunner, error) {
	if executor == nil || settings.Binary == "" || !immutableImagePattern.MatchString(settings.Image) || settings.CPUs == "" || settings.Memory == "" || settings.TmpSize == "" || settings.PIDs <= 0 || settings.DependencyTimeout <= 0 || !strings.HasPrefix(settings.Proxy, "https://") {
		return DockerRunner{}, fmt.Errorf("invalid docker checker settings")
	}
	return DockerRunner{executor: executor, settings: settings}, nil
}

// Prepare 只运行可信 Go 命令下载依赖，容器不接收宿主机凭据。
func (runner DockerRunner) Prepare(ctx context.Context, sourceDir, cacheDir string) (CommandResult, error) {
	arguments, err := runner.arguments(sourceDir, cacheDir, false)
	if err != nil {
		return CommandResult{}, err
	}
	arguments = append(arguments, "--network", "bridge", "-e", "GOPROXY="+runner.settings.Proxy, "-e", "GOSUMDB=sum.golang.org", "-e", "GONOSUMDB=", "-e", "GOPRIVATE=", "-e", "GOTOOLCHAIN=local", runner.settings.Image, "go", "mod", "download")
	callCtx, cancel := context.WithTimeout(ctx, runner.settings.DependencyTimeout)
	defer cancel()
	return runner.executor.Execute(callCtx, runner.settings.Binary, arguments)
}

// Run 在无网络、只读源码和只读依赖缓存中执行声明式注册的 Checker。
func (runner DockerRunner) Run(ctx context.Context, sourceDir, cacheDir string, definition Definition) (CommandResult, error) {
	commands := map[string][]string{"go_vet": {"go", "vet", "./..."}, "staticcheck": {"staticcheck", "./..."}}
	command, ok := commands[definition.Implementation]
	if !ok || definition.Name == "" || definition.Timeout <= 0 {
		return CommandResult{}, fmt.Errorf("unknown checker implementation")
	}
	arguments, err := runner.arguments(sourceDir, cacheDir, true)
	if err != nil {
		return CommandResult{}, err
	}
	arguments = append(arguments, "--network", "none", runner.settings.Image)
	arguments = append(arguments, command...)
	callCtx, cancel := context.WithTimeout(ctx, definition.Timeout)
	defer cancel()
	return runner.executor.Execute(callCtx, runner.settings.Binary, arguments)
}

func (runner DockerRunner) arguments(sourceDir, cacheDir string, cacheReadOnly bool) ([]string, error) {
	if !filepath.IsAbs(sourceDir) || !filepath.IsAbs(cacheDir) {
		return nil, fmt.Errorf("checker mount paths must be absolute")
	}
	cacheMode := "rw"
	if cacheReadOnly {
		cacheMode = "ro"
	}
	return []string{"run", "--rm", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", strconv.Itoa(runner.settings.PIDs), "--memory", runner.settings.Memory, "--cpus", runner.settings.CPUs, "--user", "65532:65532", "--workdir", "/workspace", "--tmpfs", "/tmp:rw,noexec,nosuid,size=" + runner.settings.TmpSize, "-e", "CGO_ENABLED=0", "-e", "GOCACHE=/tmp/go-build", "-e", "XDG_CACHE_HOME=/tmp/staticcheck", "-v", sourceDir + ":/workspace:ro", "-v", cacheDir + ":/go/pkg/mod:" + cacheMode}, nil
}

type OSExecutor struct{}

func (OSExecutor) Execute(ctx context.Context, binary string, arguments []string) (CommandResult, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if err == nil {
		return CommandResult{Output: output.String()}, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return CommandResult{Output: output.String(), ExitCode: exitError.ExitCode()}, nil
	}
	return CommandResult{}, fmt.Errorf("execute checker container: %w", err)
}
