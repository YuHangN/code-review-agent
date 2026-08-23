package checker_test

import (
	"testing"

	"github.com/YuHangN/code-review-agent/internal/checker"
	"github.com/YuHangN/code-review-agent/internal/domain"
)

func TestParseDiagnosticsKeepsOnlyPRAddedLines(t *testing.T) {
	units := []domain.ReviewUnit{{
		ID: "unit-1", FilePath: "main.go",
		DiffHunk: "@@ -3,2 +3,3 @@\n old\n+fmt.Printf(\"%d\", \"x\")\n unchanged\n",
	}}
	output := "./main.go:4:2: fmt.Printf format %d has arg of wrong type string\n" +
		"main.go:3:1: old problem (SA1000)\n" +
		"../outside.go:4:1: unsafe path (SA1001)\n"

	got := checker.ParseDiagnostics("go_vet", output, units)
	if len(got) != 1 || got[0].UnitID != "unit-1" || got[0].File != "main.go" || got[0].Line != 4 || got[0].Code != "go_vet" {
		t.Fatalf("diagnostics = %#v", got)
	}
}

func TestParseStaticcheckCode(t *testing.T) {
	units := []domain.ReviewUnit{{ID: "unit-1", FilePath: "main.go", DiffHunk: "@@ -0,0 +1 @@\n+for true {}\n"}}
	got := checker.ParseDiagnostics("staticcheck", "main.go:1:1: should use for {} instead of for true {} (S1006)\n", units)
	if len(got) != 1 || got[0].Code != "S1006" {
		t.Fatalf("diagnostics = %#v", got)
	}
}
