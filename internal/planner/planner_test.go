package planner_test

import (
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/planner"
)

func TestPlanCreatesDeterministicUnitsAndSkipsLowValueFiles(t *testing.T) {
	diff := "diff --git a/internal/auth/login.go b/internal/auth/login.go\n@@ -1,2 +1,4 @@\n+token := input\n" +
		"diff --git a/go.sum b/go.sum\n@@ -1 +1 @@\n+example\n"
	request := planner.Request{RunID: "run-1", HeadSHA: "head", Diff: diff, Now: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)}
	first := planner.New().Plan(request)
	second := planner.New().Plan(request)
	if len(first) != 1 || first[0].FilePath != "internal/auth/login.go" || first[0].Risk != "high" {
		t.Fatalf("units = %#v", first)
	}
	if first[0].UnitKey != second[0].UnitKey || first[0].Status != domain.UnitStatusPending {
		t.Fatalf("units are not deterministic: %#v %#v", first[0], second[0])
	}
}

func TestPlanSkipsSecurityExcludedFileBlocks(t *testing.T) {
	diff := "diff --git a/.env b/.env\n<REDACTED_FILE:.env>\n" +
		"diff --git a/main.go b/main.go\n@@ -1 +1 @@\n+fmt.Println(\"safe\")\n"
	units := planner.New().Plan(planner.Request{RunID: "run-1", HeadSHA: "head", Diff: diff, Now: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)})
	if len(units) != 1 || units[0].FilePath != "main.go" {
		t.Fatalf("units = %#v, want only main.go", units)
	}
}
