package verifier_test

import (
	"testing"
	"time"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/verifier"
)

func TestVerifierConfirmsHardcodedSecretWithDeterministicRule(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	candidate := testCandidate("candidate-secret", "security", 2, "配置中包含硬编码 API Key")
	unit := domain.ReviewUnit{
		ID: "unit-secret", RunID: candidate.RunID, FilePath: candidate.File,
		DiffHunk: "@@ -0,0 +1,2 @@\n+package config\n+api_key: \"<REDACTED:API_KEY:1>\"\n",
	}

	finding := verifier.NewDefault().Verify(candidate, unit, now)

	if finding.Confidence != domain.ConfidenceConfirmed {
		t.Fatalf("confidence = %q, want %q", finding.Confidence, domain.ConfidenceConfirmed)
	}
	if finding.VerificationSource != "rule:redacted_secret_assignment" {
		t.Fatalf("verification source = %q", finding.VerificationSource)
	}
	if finding.TraceID != candidate.TraceID || finding.CandidateID != candidate.ID {
		t.Fatalf("finding lost evidence links: %#v", finding)
	}
	if finding.Fingerprint == "" || finding.ID == "" {
		t.Fatalf("finding identity is empty: %#v", finding)
	}
}

func TestVerifierKeepsLLMOnlyFindingAdvisory(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	candidate := testCandidate("candidate-concurrency", "concurrency", 6, "共享 map 可能发生并发访问")
	unit := domain.ReviewUnit{
		ID: "unit-cache", RunID: candidate.RunID, FilePath: candidate.File,
		DiffHunk: "@@ -0,0 +1,6 @@\n+package cache\n+\n+var cache = map[string]string{}\n+\n+func Update() {\n+ go func() { cache[\"k\"] = \"v\" }()\n",
	}

	finding := verifier.NewDefault().Verify(candidate, unit, now)

	if finding.Confidence != domain.ConfidenceAdvisory {
		t.Fatalf("confidence = %q, want %q", finding.Confidence, domain.ConfidenceAdvisory)
	}
	if finding.VerificationSource != "llm_reasoning_only" {
		t.Fatalf("verification source = %q", finding.VerificationSource)
	}
}

func TestVerifierRejectsToolEvidenceThatDoesNotMatchAddedLine(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 30, 0, 0, time.UTC)
	candidate := testCandidate("candidate-secret", "security", 2, "配置中包含硬编码 API Key")
	unit := domain.ReviewUnit{
		ID: "unit-secret", RunID: candidate.RunID, FilePath: candidate.File,
		DiffHunk: "@@ -0,0 +1,2 @@\n+package config\n+api_key: os.Getenv(\"API_KEY\")\n",
	}
	steps := []domain.AgentStep{{
		RunID: candidate.RunID, UnitID: candidate.UnitID, Round: 1,
		ToolCalls: []domain.AgentToolCall{{ID: "tool-1", Name: "read_file", Arguments: `{"path":"config.yaml"}`}},
		ToolResults: []domain.AgentToolResult{{
			CallID: "tool-1", Name: "read_file",
			Content: `{"path":"config.yaml","sha":"head-sha","content":"package config\napi_key: \"<REDACTED:API_KEY:1>\"\n","redactions":1}`,
		}},
	}}

	finding := verifier.NewDefault().VerifyWithEvidence(candidate, unit, steps, now)

	if finding.Confidence != domain.ConfidenceAdvisory {
		t.Fatalf("confidence = %q, want %q", finding.Confidence, domain.ConfidenceAdvisory)
	}
}

func TestVerifierGeneratesSameFingerprintForDuplicateCandidates(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	first := testCandidate("candidate-a", "security", 2, "配置中包含硬编码 API Key")
	second := testCandidate("candidate-b", "security", 2, "配置中包含硬编码 API Key")
	unit := domain.ReviewUnit{ID: "unit-secret", RunID: first.RunID, FilePath: first.File, DiffHunk: "@@ -0,0 +1,2 @@\n+package config\n+api_key: \"<REDACTED:API_KEY:1>\"\n"}
	checker := verifier.NewDefault()

	left := checker.Verify(first, unit, now)
	right := checker.Verify(second, unit, now)

	if left.Fingerprint != right.Fingerprint || left.ID != right.ID {
		t.Fatalf("duplicate identities differ: (%q, %q) and (%q, %q)", left.ID, left.Fingerprint, right.ID, right.Fingerprint)
	}
}

func TestVerifierScopesFindingIDToRun(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	first := testCandidate("candidate-a", "security", 2, "配置中包含硬编码 API Key")
	second := first
	second.ID = "candidate-b"
	second.RunID = "run-002"
	unitA := domain.ReviewUnit{ID: "unit-a", RunID: first.RunID, FilePath: first.File, DiffHunk: "@@ -0,0 +1,2 @@\n+package config\n+api_key: \"<REDACTED:API_KEY:1>\"\n"}
	unitB := unitA
	unitB.ID = "unit-b"
	unitB.RunID = second.RunID
	checker := verifier.NewDefault()

	left := checker.Verify(first, unitA, now)
	right := checker.Verify(second, unitB, now)

	if left.Fingerprint != right.Fingerprint {
		t.Fatalf("same issue fingerprints differ: %q and %q", left.Fingerprint, right.Fingerprint)
	}
	if left.ID == right.ID {
		t.Fatalf("finding ID %q is not scoped to its run", left.ID)
	}
}

func testCandidate(id, category string, line int, title string) domain.CandidateFindingRecord {
	return domain.CandidateFindingRecord{
		ID: id, RunID: "run-001", UnitID: "unit-001", TraceID: "trace-001",
		Detector: "llm_review", Category: category, Severity: "high",
		File: "config.yaml", Line: line, Title: title,
		Explanation: "模型给出的候选问题", Evidence: []string{"候选证据"}, Suggestion: "修改配置",
	}
}
