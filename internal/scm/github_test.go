package scm_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/scm"
)

func TestParseGitHubPullRequestURL(t *testing.T) {
	ref, err := scm.ParseGitHubPullRequestURL("https://github.com/acme/payments/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Owner != "acme" || ref.Repository != "payments" || ref.Number != 42 {
		t.Fatalf("parsed ref = %#v, want acme/payments#42", ref)
	}
}

func TestParseGitHubPullRequestURLRejectsUnsupportedURL(t *testing.T) {
	if _, err := scm.ParseGitHubPullRequestURL("https://gitlab.com/acme/payments/-/merge_requests/42"); err == nil {
		t.Fatal("ParseGitHubPullRequestURL succeeded, want error")
	}
}

func TestGitHubAdapterFetchesDiffForPinnedSHAs(t *testing.T) {
	const diff = "diff --git a/cache.go b/cache.go\n+new line\n"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/payments/pulls/42":
			if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Fatalf("authorization = %q, want bearer token", got)
			}
			if got := request.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Fatalf("metadata accept = %q", got)
			}
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"base":{"sha":"base-sha"},"head":{"sha":"head-sha"}}`)
		case "/repos/acme/payments/compare/base-sha...head-sha":
			if got := request.Header.Get("Accept"); got != "application/vnd.github.diff" {
				t.Fatalf("diff accept = %q", got)
			}
			fmt.Fprint(writer, diff)
		default:
			t.Fatalf("unexpected request path: %s", request.URL.Path)
		}
	}))
	defer server.Close()

	adapter, err := scm.NewGitHubAdapter(server.Client(), server.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Fetch(context.Background(), scm.PullRequestRef{Owner: "acme", Repository: "payments", Number: 42})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseSHA != "base-sha" || snapshot.HeadSHA != "head-sha" || snapshot.Diff != diff {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(diff)))
	if snapshot.DiffSHA256 != wantHash {
		t.Fatalf("diff hash = %q, want %q", snapshot.DiffSHA256, wantHash)
	}
}
