package checker_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/checker"
	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/store/sqlite"
)

func TestProcessorCheckpointsBothCheckersAndMapsAddedLines(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.Options{BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	run := domain.Run{ID: "run-1", SourceURL: "https://github.com/acme/demo/pull/1", Provider: "github", Repository: "acme/demo", ChangeNumber: 1, Status: domain.RunStatusChecking, BudgetLimitMicros: 1, CreatedAt: now, UpdatedAt: now}
	unit := domain.ReviewUnit{ID: "unit-1", RunID: run.ID, UnitKey: "main.go#1", FilePath: "main.go", DiffHunk: "@@ -0,0 +1 @@\n+package main\n", Risk: "medium", Status: domain.UnitStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateRun(ctx, run, []domain.ReviewUnit{unit}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(ctx, run.ID, domain.ChangeSnapshot{BaseSHA: "base", HeadSHA: "head", Diff: unit.DiffHunk, DiffSHA256: "hash", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimRun(ctx, run.ID, "worker", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	archive := tarGzip(t, []tarEntry{{name: "repo-head/go.mod", body: "module example.test/demo\n"}, {name: "repo-head/main.go", body: "package main\n"}})
	runner := &fakeRunner{outputs: map[string]checker.CommandResult{
		"go_vet":      {Output: "main.go:1:1: vet problem\n", ExitCode: 1},
		"staticcheck": {Output: "main.go:1:1: static problem (SA1000)\n", ExitCode: 1},
	}}
	processor := checker.NewProcessor(store, archiveSource{data: archive}, runner, []checker.Definition{{Name: "go_vet", Implementation: "go_vet", Timeout: time.Minute}, {Name: "staticcheck", Implementation: "staticcheck", Timeout: time.Minute}})
	if err := processor.Process(ctx, run.ID, "worker", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := store.ListCheckerDiagnostics(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 2 || runner.prepareCalls != 1 || runner.runCalls != 2 {
		t.Fatalf("diagnostics=%#v prepare=%d run=%d", diagnostics, runner.prepareCalls, runner.runCalls)
	}
	if err := processor.Process(ctx, run.ID, "worker", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if runner.prepareCalls != 1 || runner.runCalls != 2 {
		t.Fatalf("completed checkers ran again: prepare=%d run=%d", runner.prepareCalls, runner.runCalls)
	}
}

type archiveSource struct{ data []byte }

func (source archiveSource) OpenArchive(context.Context, domain.Run, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(source.data)), nil
}

type fakeRunner struct {
	outputs                map[string]checker.CommandResult
	prepareCalls, runCalls int
}

func (runner *fakeRunner) Prepare(context.Context, string, string) (checker.CommandResult, error) {
	runner.prepareCalls++
	return checker.CommandResult{}, nil
}
func (runner *fakeRunner) Run(_ context.Context, _, _ string, definition checker.Definition) (checker.CommandResult, error) {
	runner.runCalls++
	return runner.outputs[definition.Name], nil
}
