package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	ErrInvalidOpenAIConfig   = errors.New("invalid OpenAI provider configuration")
	ErrInvalidOpenAIResponse = errors.New("invalid OpenAI response")
)

const maxOpenAIResponseBytes = 4 << 20

// OpenAIProvider 使用 Responses API 实现统一 Provider 接口。
type OpenAIProvider struct {
	client   *http.Client
	endpoint string
	apiKey   string
}

func NewOpenAIProvider(client *http.Client, baseURL, apiKey string) (*OpenAIProvider, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if client == nil || baseURL == "" || strings.TrimSpace(apiKey) == "" {
		return nil, ErrInvalidOpenAIConfig
	}
	endpoint := baseURL + "/v1/responses"
	if strings.HasSuffix(baseURL, "/v1") {
		endpoint = baseURL + "/responses"
	}
	return &OpenAIProvider{client: client, endpoint: endpoint, apiKey: apiKey}, nil
}

// Generate 发送一次非流式 Responses API 请求，并返回可用于预算结算的 usage。
func (provider *OpenAIProvider) Generate(ctx context.Context, request GenerateRequest) (Response, error) {
	if provider == nil || provider.client == nil || request.Model == "" || strings.TrimSpace(request.Prompt) == "" || request.MaxOutputTokens <= 0 {
		return Response{}, ErrInvalidCall
	}
	payload, err := json.Marshal(struct {
		Model           string `json:"model"`
		Input           string `json:"input"`
		MaxOutputTokens int64  `json:"max_output_tokens"`
		Store           bool   `json:"store"`
	}{Model: request.Model, Input: request.Prompt, MaxOutputTokens: request.MaxOutputTokens, Store: false})
	if err != nil {
		return Response{}, fmt.Errorf("encode OpenAI request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("create OpenAI request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+provider.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := provider.client.Do(httpRequest)
	if err != nil {
		return Response{}, fmt.Errorf("send OpenAI request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOpenAIResponseBytes))
	if err != nil {
		return Response{}, fmt.Errorf("read OpenAI response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		message := strings.TrimSpace(failure.Error.Message)
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return Response{}, fmt.Errorf("OpenAI API status %d: %s", response.StatusCode, message)
	}

	var decoded struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Response{}, fmt.Errorf("%w: decode body: %v", ErrInvalidOpenAIResponse, err)
	}
	if decoded.Status != "completed" {
		reason := decoded.Status
		if decoded.IncompleteDetails != nil && decoded.IncompleteDetails.Reason != "" {
			reason += ": " + decoded.IncompleteDetails.Reason
		}
		if decoded.Error != nil && decoded.Error.Message != "" {
			reason += ": " + decoded.Error.Message
		}
		return Response{}, fmt.Errorf("%w: response status %s", ErrInvalidOpenAIResponse, reason)
	}
	var texts []string
	for _, item := range decoded.Output {
		if item.Type != "message" {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "output_text" && content.Text != "" {
				texts = append(texts, content.Text)
			}
		}
	}
	if len(texts) == 0 {
		return Response{}, fmt.Errorf("%w: output_text is empty", ErrInvalidOpenAIResponse)
	}
	result := Response{Content: strings.Join(texts, "\n")}
	if decoded.Usage != nil {
		result.Usage = &TokenUsage{InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens}
	}
	return result, nil
}
