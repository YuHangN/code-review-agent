// Package llm 提供带预算保护的统一模型调用入口。
package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/YuHangN/code-review-agent/internal/budget"
)

const tokensPerMillion = int64(1_000_000)

var (
	ErrInvalidCall             = errors.New("invalid LLM call")
	ErrUnknownTier             = errors.New("unknown LLM tier")
	ErrUnknownProvider         = errors.New("unknown LLM provider")
	ErrUsageExceedsReservation = errors.New("provider usage exceeds reservation")
)

// Tier 描述一个可选模型层级及其费用上界参数。
type Tier struct {
	Provider                    string
	Model                       string
	InputPriceMicrosPerMillion  int64
	OutputPriceMicrosPerMillion int64
	MaxOutputTokens             int64
}

// CallRequest 是一次可独立记账的模型调用。
type CallRequest struct {
	ID     string
	RunID  string
	UnitID string
	Tier   string
	Prompt string
}

// GenerateRequest 是传给具体 Provider 的统一请求。
type GenerateRequest struct {
	Model           string
	Prompt          string
	MaxOutputTokens int64
}

// TokenUsage 是 Provider 返回的真实 token 用量。
type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
}

// Response 保存模型文本以及可选的真实用量。
type Response struct {
	Content string
	Usage   *TokenUsage
}

// Provider 隔离不同模型服务的 API 差异。
type Provider interface {
	Generate(ctx context.Context, request GenerateRequest) (Response, error)
}

// TokenCounter 在调用前计算输入 token 上界。
type TokenCounter interface {
	CountInputTokens(ctx context.Context, model, prompt string) (int64, error)
}

// BudgetLedger 是 Gateway 使用的预算预留与结算能力。
type BudgetLedger interface {
	Reserve(ctx context.Context, reservation budget.Reservation) error
	Settle(ctx context.Context, reservationID string, usage budget.Usage) error
	Release(ctx context.Context, reservationID string) error
}

// Gateway 保证只有成功预留预算的请求才能到达模型 Provider。
type Gateway struct {
	ledger    BudgetLedger
	counter   TokenCounter
	providers map[string]Provider
	tiers     map[string]Tier
}

func NewGateway(ledger BudgetLedger, counter TokenCounter, providers map[string]Provider, tiers map[string]Tier) Gateway {
	return Gateway{ledger: ledger, counter: counter, providers: providers, tiers: tiers}
}

// Call 完成“计算上界、预留、调用、结算或释放”的单次预算闭环。
func (gateway Gateway) Call(ctx context.Context, request CallRequest) (Response, error) {
	if request.ID == "" || request.RunID == "" || request.UnitID == "" || request.Tier == "" || strings.TrimSpace(request.Prompt) == "" || gateway.ledger == nil || gateway.counter == nil {
		return Response{}, ErrInvalidCall
	}
	tier, ok := gateway.tiers[request.Tier]
	if !ok {
		return Response{}, fmt.Errorf("%w: %s", ErrUnknownTier, request.Tier)
	}
	if err := validateTier(tier); err != nil {
		return Response{}, err
	}
	provider, ok := gateway.providers[tier.Provider]
	if !ok || provider == nil {
		return Response{}, fmt.Errorf("%w: %s", ErrUnknownProvider, tier.Provider)
	}

	inputUpperBound, err := gateway.counter.CountInputTokens(ctx, tier.Model, request.Prompt)
	if err != nil {
		return Response{}, fmt.Errorf("count input tokens: %w", err)
	}
	reservedMicros, err := tier.cost(inputUpperBound, tier.MaxOutputTokens)
	if err != nil || inputUpperBound < 0 || reservedMicros <= 0 {
		return Response{}, fmt.Errorf("estimate reservation: %w", ErrInvalidCall)
	}
	reservation := budget.Reservation{
		ID:             request.ID,
		RunID:          request.RunID,
		UnitID:         request.UnitID,
		Tier:           request.Tier,
		ReservedMicros: reservedMicros,
		CreatedAt:      time.Now().UTC(),
	}
	if err := gateway.ledger.Reserve(ctx, reservation); err != nil {
		return Response{}, err
	}

	response, err := provider.Generate(ctx, GenerateRequest{Model: tier.Model, Prompt: request.Prompt, MaxOutputTokens: tier.MaxOutputTokens})
	if err != nil {
		if releaseErr := gateway.ledger.Release(ctx, request.ID); releaseErr != nil {
			return Response{}, errors.Join(fmt.Errorf("generate: %w", err), fmt.Errorf("release reservation: %w", releaseErr))
		}
		return Response{}, fmt.Errorf("generate: %w", err)
	}

	usage := budget.Usage{ActualMicros: reservedMicros, InputTokens: inputUpperBound, OutputTokens: tier.MaxOutputTokens}
	if response.Usage != nil {
		if response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 || response.Usage.InputTokens > inputUpperBound || response.Usage.OutputTokens > tier.MaxOutputTokens {
			if settleErr := gateway.ledger.Settle(ctx, request.ID, usage); settleErr != nil {
				return Response{}, errors.Join(ErrUsageExceedsReservation, settleErr)
			}
			return Response{}, ErrUsageExceedsReservation
		}
		actualMicros, costErr := tier.cost(response.Usage.InputTokens, response.Usage.OutputTokens)
		if costErr != nil {
			return Response{}, fmt.Errorf("calculate actual cost: %w", costErr)
		}
		usage = budget.Usage{ActualMicros: actualMicros, InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens}
	}
	if err := gateway.ledger.Settle(ctx, request.ID, usage); err != nil {
		return Response{}, fmt.Errorf("settle model call: %w", err)
	}
	return response, nil
}

func validateTier(tier Tier) error {
	if tier.Provider == "" || tier.Model == "" || tier.InputPriceMicrosPerMillion <= 0 || tier.OutputPriceMicrosPerMillion <= 0 || tier.MaxOutputTokens <= 0 {
		return fmt.Errorf("%w: invalid tier configuration", ErrInvalidCall)
	}
	return nil
}

func (tier Tier) cost(inputTokens, outputTokens int64) (int64, error) {
	inputCost, err := tokenCost(inputTokens, tier.InputPriceMicrosPerMillion)
	if err != nil {
		return 0, err
	}
	outputCost, err := tokenCost(outputTokens, tier.OutputPriceMicrosPerMillion)
	if err != nil || inputCost > math.MaxInt64-outputCost {
		return 0, fmt.Errorf("token cost overflows int64")
	}
	return inputCost + outputCost, nil
}

// tokenCost 向上取整到微美元，避免小额调用因为整数除法而少预留。
func tokenCost(tokens, priceMicrosPerMillion int64) (int64, error) {
	if tokens < 0 || priceMicrosPerMillion <= 0 || (tokens != 0 && priceMicrosPerMillion > math.MaxInt64/tokens) {
		return 0, fmt.Errorf("invalid or overflowing token cost")
	}
	product := tokens * priceMicrosPerMillion
	cost := product / tokensPerMillion
	if product%tokensPerMillion != 0 {
		cost++
	}
	return cost, nil
}
