package checker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/security"
)

type ProcessorStore interface {
	GetRun(context.Context, string) (domain.Run, error)
	GetSnapshot(context.Context, string) (domain.ChangeSnapshot, error)
	ListUnits(context.Context, string) ([]domain.ReviewUnit, error)
	EnsureCheckerRuns(context.Context, string, []string, time.Time) error
	ListCheckerRuns(context.Context, string) ([]domain.CheckerRun, error)
	ClaimCheckerRun(context.Context, string, string, string, time.Time) (domain.CheckerRun, error)
	CompleteCheckerRun(context.Context, domain.CheckerRun, []domain.CheckerDiagnostic, []domain.ReviewTrace, string, time.Time) error
	FailCheckerRun(context.Context, domain.CheckerRun, string, string, time.Time) error
}

type ArchiveSource interface {
	OpenArchive(context.Context, domain.Run, string) (io.ReadCloser, error)
}

type Runner interface {
	Prepare(context.Context, string, string) (CommandResult, error)
	Run(context.Context, string, string, Definition) (CommandResult, error)
}

type Processor struct {
	store       ProcessorStore
	source      ArchiveSource
	runner      Runner
	definitions []Definition
}

func NewProcessor(store ProcessorStore, source ArchiveSource, runner Runner, definitions []Definition) Processor {
	return Processor{store: store, source: source, runner: runner, definitions: append([]Definition(nil), definitions...)}
}

func (processor Processor) Process(ctx context.Context, runID, owner string, now time.Time) error {
	if processor.store == nil || processor.source == nil || processor.runner == nil || runID == "" || owner == "" || len(processor.definitions) == 0 {
		return fmt.Errorf("invalid checker processor")
	}
	names := make([]string, len(processor.definitions))
	definitions := make(map[string]Definition, len(processor.definitions))
	for index, definition := range processor.definitions {
		names[index], definitions[definition.Name] = definition.Name, definition
	}
	if err := processor.store.EnsureCheckerRuns(ctx, runID, names, now); err != nil {
		return err
	}
	checkpoints, err := processor.store.ListCheckerRuns(ctx, runID)
	if err != nil {
		return err
	}
	pending := false
	for _, checkpoint := range checkpoints {
		if checkpoint.Status != domain.CheckerStatusCompleted {
			pending = true
		}
	}
	if !pending {
		return nil
	}
	run, err := processor.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	snapshot, err := processor.store.GetSnapshot(ctx, runID)
	if err != nil {
		return err
	}
	units, err := processor.store.ListUnits(ctx, runID)
	if err != nil {
		return err
	}
	workspace, err := os.MkdirTemp("", "review-agent-checker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	sourceDir, cacheDir := filepath.Join(workspace, "source"), filepath.Join(workspace, "gomodcache")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return err
	}
	archive, err := processor.source.OpenArchive(ctx, run, snapshot.HeadSHA)
	if err != nil {
		return err
	}
	err = ExtractArchive(archive, sourceDir)
	closeErr := archive.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	prepared, err := processor.runner.Prepare(ctx, sourceDir, cacheDir)
	if err != nil {
		return err
	}
	if prepared.ExitCode != 0 {
		return fmt.Errorf("prepare Go dependencies exited %d: %s", prepared.ExitCode, sanitize(prepared.Output))
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.Status == domain.CheckerStatusCompleted {
			continue
		}
		definition, ok := definitions[checkpoint.Checker]
		if !ok {
			return fmt.Errorf("checker %q is not configured", checkpoint.Checker)
		}
		claimed, err := processor.store.ClaimCheckerRun(ctx, runID, checkpoint.Checker, owner, time.Now().UTC())
		if err != nil {
			return err
		}
		result, runErr := processor.runner.Run(ctx, sourceDir, cacheDir, definition)
		if runErr != nil {
			_ = processor.store.FailCheckerRun(context.WithoutCancel(ctx), claimed, sanitize(runErr.Error()), owner, time.Now().UTC())
			return runErr
		}
		result.Output = sanitize(result.Output)
		parsed := ParseDiagnostics(definition.Name, result.Output, units)
		diagnostics := make([]domain.CheckerDiagnostic, 0, len(parsed))
		traces := make([]domain.ReviewTrace, 0, len(parsed))
		for _, item := range parsed {
			hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", claimed.ID, item.File, item.Line, item.Code)))
			suffix := fmt.Sprintf("%x", hash[:8])
			traceID := "trace-checker-" + suffix
			response, _ := json.Marshal(item)
			traces = append(traces, domain.ReviewTrace{ID: traceID, RunID: runID, UnitID: item.UnitID, CallID: "checker-call-" + suffix, Detector: "checker:" + definition.Name, Status: "completed", Response: string(response), CreatedAt: now})
			diagnostics = append(diagnostics, domain.CheckerDiagnostic{ID: "diagnostic-" + suffix, RunID: runID, CheckerRunID: claimed.ID, UnitID: item.UnitID, TraceID: traceID, Checker: definition.Name, File: item.File, Line: item.Line, Column: item.Column, Code: item.Code, Message: item.Message, Severity: "high", CreatedAt: now})
		}
		claimed.Command = commandFor(definition.Implementation)
		claimed.ExitCode = result.ExitCode
		claimed.Output = result.Output
		if err := processor.store.CompleteCheckerRun(ctx, claimed, diagnostics, traces, owner, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func commandFor(implementation string) []string {
	if implementation == "go_vet" {
		return []string{"go", "vet", "./..."}
	}
	if implementation == "staticcheck" {
		return []string{"staticcheck", "./..."}
	}
	return nil
}

func sanitize(value string) string {
	return security.NewSanitizer().SanitizeSnapshot(domain.ChangeSnapshot{Diff: value}).Snapshot.Diff
}
