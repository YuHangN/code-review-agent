package security_test

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/domain"
	"github.com/YuHangN/code-review-agent/internal/security"
)

func TestSanitizeSnapshotRedactsSecretValuesAndRehashesDiff(t *testing.T) {
	const secret = "sk-live-1234567890abcdef"
	input := domain.ChangeSnapshot{
		Diff: "diff --git a/config.yaml b/config.yaml\n" +
			"+++ b/config.yaml\n" +
			"+api_key: \"" + secret + "\"\n",
		DiffSHA256: "raw-hash",
	}

	result := security.NewSanitizer().SanitizeSnapshot(input)
	if strings.Contains(result.Snapshot.Diff, secret) {
		t.Fatalf("sanitized diff contains secret: %q", result.Snapshot.Diff)
	}
	if !strings.Contains(result.Snapshot.Diff, "<REDACTED:API_KEY:1>") {
		t.Fatalf("sanitized diff = %q, want tracked placeholder", result.Snapshot.Diff)
	}
	if len(result.Redactions) != 1 || result.Redactions[0].Kind != "API_KEY" {
		t.Fatalf("redactions = %#v, want one API_KEY redaction", result.Redactions)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(result.Snapshot.Diff)))
	if result.Snapshot.DiffSHA256 != wantHash {
		t.Fatalf("diff hash = %q, want %q", result.Snapshot.DiffSHA256, wantHash)
	}
}

func TestSanitizeSnapshotExcludesSensitiveFileBlocks(t *testing.T) {
	const privateKey = "PRIVATE-KEY-MATERIAL"
	input := domain.ChangeSnapshot{Diff: "diff --git a/.env b/.env\n" +
		"+++ b/.env\n" +
		"+DATABASE_PASSWORD=" + privateKey + "\n" +
		"diff --git a/main.go b/main.go\n" +
		"+++ b/main.go\n" +
		"+fmt.Println(\"safe\")\n"}

	result := security.NewSanitizer().SanitizeSnapshot(input)
	if strings.Contains(result.Snapshot.Diff, privateKey) {
		t.Fatalf("sanitized diff contains excluded file content: %q", result.Snapshot.Diff)
	}
	if !strings.Contains(result.Snapshot.Diff, "<REDACTED_FILE:.env>") {
		t.Fatalf("sanitized diff = %q, want excluded file marker", result.Snapshot.Diff)
	}
	if !strings.Contains(result.Snapshot.Diff, "fmt.Println(\"safe\")") {
		t.Fatalf("sanitized diff dropped safe file: %q", result.Snapshot.Diff)
	}
	if len(result.ExcludedFiles) != 1 || result.ExcludedFiles[0] != ".env" {
		t.Fatalf("excluded files = %v, want [.env]", result.ExcludedFiles)
	}
}
