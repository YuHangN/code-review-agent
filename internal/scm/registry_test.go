package scm_test

import (
	"errors"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/scm"
)

func TestRegistryResolvesGitHubPullRequestURL(t *testing.T) {
	adapter, err := scm.NewGitHubAdapter(nil, "https://api.github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := scm.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := registry.ResolveURL("https://github.com/acme/payments/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Ref.Provider != "github" || resolved.Ref.Repository != "acme/payments" || resolved.Ref.Number != 42 {
		t.Fatalf("resolved ref = %#v, want github acme/payments#42", resolved.Ref)
	}
	if resolved.Adapter.Provider() != "github" {
		t.Fatalf("adapter provider = %q, want github", resolved.Adapter.Provider())
	}
}

func TestRegistryRestoresAdapterByPersistedProvider(t *testing.T) {
	adapter, err := scm.NewGitHubAdapter(nil, "https://api.github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := scm.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}

	restored, err := registry.Adapter("github")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Provider() != "github" {
		t.Fatalf("adapter provider = %q, want github", restored.Provider())
	}
}

func TestRegistryRejectsGitLabUntilAdapterIsRegistered(t *testing.T) {
	adapter, err := scm.NewGitHubAdapter(nil, "https://api.github.com", "")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := scm.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}

	_, err = registry.ResolveURL("https://gitlab.com/acme/payments/-/merge_requests/42")
	if !errors.Is(err, scm.ErrUnsupportedURL) {
		t.Fatalf("ResolveURL error = %v, want ErrUnsupportedURL", err)
	}
}
