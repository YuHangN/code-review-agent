package prompts_test

import (
	"strings"
	"testing"

	"github.com/YuHangN/code-review-agent/prompts"
)

func TestRenderReviewUsesEmbeddedTemplate(t *testing.T) {
	result, err := prompts.RenderReview(prompts.ReviewData{
		MaxFindings: 3,
		FilePath:    "internal/cache/cache.go",
		Risk:        "high",
		Diff:        "@@ -0,0 +1 @@\n+var cache = map[string]string{}\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"最多返回 3 条",
		`file 必须是 "internal/cache/cache.go"`,
		`<review_unit file="internal/cache/cache.go" risk="high">`,
		"+var cache = map[string]string{}",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("rendered prompt does not contain %q:\n%s", want, result)
		}
	}
}
