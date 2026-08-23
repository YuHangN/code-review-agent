package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/llm"
)

func TestOpenAIProviderGeneratesTextAndUsageWithResponsesAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q", got)
		}
		var body struct {
			Model           string `json:"model"`
			Input           string `json:"input"`
			MaxOutputTokens int64  `json:"max_output_tokens"`
			Store           bool   `json:"store"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "gpt-test" || body.Input != "review prompt" || body.MaxOutputTokens != 321 || body.Store {
			t.Fatalf("request body = %#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"id":"resp-1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"findings\":[]}"}]}],"usage":{"input_tokens":27,"output_tokens":9,"total_tokens":36}}`)
	}))
	defer server.Close()
	provider, err := llm.NewOpenAIProvider(server.Client(), server.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}

	response, err := provider.Generate(context.Background(), llm.GenerateRequest{Model: "gpt-test", Prompt: "review prompt", MaxOutputTokens: 321})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != `{"findings":[]}` || response.Usage == nil || response.Usage.InputTokens != 27 || response.Usage.OutputTokens != 9 {
		t.Fatalf("response = %#v", response)
	}
}

func TestOpenAIProviderRejectsIncompleteOrFailedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"id":"resp-1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":27,"output_tokens":10}}`)
	}))
	defer server.Close()
	provider, err := llm.NewOpenAIProvider(server.Client(), server.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.Generate(context.Background(), llm.GenerateRequest{Model: "gpt-test", Prompt: "review prompt", MaxOutputTokens: 10}); err == nil {
		t.Fatal("incomplete response was accepted")
	}
}
