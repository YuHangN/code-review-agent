package scm_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/scm"
)

func TestParseGitHubPullRequestURL(t *testing.T) {
	adapter, err := scm.NewGitHubAdapter(nil, "https://api.github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := adapter.ParseURL("https://github.com/acme/payments/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "github" || ref.Repository != "acme/payments" || ref.Number != 42 {
		t.Fatalf("parsed ref = %#v, want acme/payments#42", ref)
	}
}

func TestGitHubAdapterReadsFileAtExplicitCommitSHA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/acme/payments/contents/internal/auth/token.go" {
			t.Fatalf("request path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("ref") != "head-sha" {
			t.Fatalf("ref = %q, want head-sha", request.URL.Query().Get("ref"))
		}
		if request.Header.Get("Accept") != "application/vnd.github.raw+json" {
			t.Fatalf("accept = %q", request.Header.Get("Accept"))
		}
		fmt.Fprint(writer, "package auth\n")
	}))
	defer server.Close()
	adapter, err := scm.NewGitHubAdapter(server.Client(), server.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}

	content, err := adapter.ReadFile(context.Background(), scm.ChangeRef{Provider: "github", Repository: "acme/payments", Number: 42}, "head-sha", "internal/auth/token.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "package auth\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestGitHubAdapterRejectsUnsafeFilePathWithoutRequest(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requested = true }))
	defer server.Close()
	adapter, err := scm.NewGitHubAdapter(server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = adapter.ReadFile(context.Background(), scm.ChangeRef{Provider: "github", Repository: "acme/payments", Number: 42}, "head-sha", "../.env")
	if err == nil || !strings.Contains(err.Error(), "unsafe repository file path") {
		t.Fatalf("ReadFile error = %v", err)
	}
	if requested {
		t.Fatal("unsafe path reached GitHub API")
	}
}

func TestGitHubAdapterRejectsOversizedRepositoryFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, strings.Repeat("x", 2<<20))
	}))
	defer server.Close()
	adapter, err := scm.NewGitHubAdapter(server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = adapter.ReadFile(context.Background(), scm.ChangeRef{Provider: "github", Repository: "acme/payments", Number: 42}, "head-sha", "large.txt")
	if err == nil || !strings.Contains(err.Error(), "response body exceeds limit") {
		t.Fatalf("ReadFile error = %v", err)
	}
}

func TestParseGitHubPullRequestURLRejectsUnsupportedURL(t *testing.T) {
	adapter, err := scm.NewGitHubAdapter(nil, "https://api.github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ParseURL("https://gitlab.com/acme/payments/-/merge_requests/42"); !errors.Is(err, scm.ErrUnsupportedURL) {
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
	snapshot, err := adapter.Fetch(context.Background(), scm.ChangeRef{Provider: "github", Repository: "acme/payments", Number: 42})
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
