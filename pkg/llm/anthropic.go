package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// anthropicRefiner calls the Claude Messages API through the official SDK.
type anthropicRefiner struct {
	client anthropic.Client
	cfg    Config
}

func newAnthropic(cfg Config) *anthropicRefiner {
	// The SDK reads credentials (ANTHROPIC_API_KEY, ...) and
	// ANTHROPIC_BASE_URL from the environment itself; options below win.
	opts := []option.RequestOption{option.WithRequestTimeout(cfg.Timeout)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &anthropicRefiner{client: anthropic.NewClient(opts...), cfg: cfg}
}

func (a *anthropicRefiner) Provider() string { return ProviderAnthropic }
func (a *anthropicRefiner) Model() string    { return a.cfg.Model }

// params builds the Messages API request. Thinking is deliberately unset:
// the default model runs adaptive thinking, and disabling it is rejected.
func (a *anthropicRefiner) params(p Prompt) anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		Model:     a.cfg.Model,
		MaxTokens: a.cfg.MaxTokens,
		System:    []anthropic.TextBlockParam{{Text: p.System}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(p.User))},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(a.cfg.Effort),
			Format: anthropic.JSONOutputFormatParam{Schema: ResponseSchema()},
		},
	}
}

func (a *anthropicRefiner) Refine(ctx context.Context, c *schema.Catalog, baseline *plan.DomainPlan) (plan.Refinement, error) {
	p, err := BuildPrompt(c, baseline)
	if err != nil {
		return plan.Refinement{}, err
	}
	msg, err := a.client.Messages.New(ctx, a.params(p))
	if err != nil {
		return plan.Refinement{}, anthropicError(err)
	}

	// Stop reason first: on a refusal or truncation the content is not a
	// usable answer.
	switch msg.StopReason {
	case anthropic.StopReasonRefusal:
		if cat := string(msg.StopDetails.Category); cat != "" {
			return plan.Refinement{}, fmt.Errorf("%w (category %s)", ErrRefused, cat)
		}
		return plan.Refinement{}, ErrRefused
	case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
		return plan.Refinement{}, fmt.Errorf("%w (stop reason %s, max tokens %d)", ErrTruncated, msg.StopReason, a.cfg.MaxTokens)
	}

	var text strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" { // thinking blocks are ignored
			text.WriteString(block.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return plan.Refinement{}, fmt.Errorf("llm: anthropic: no text in response (stop reason %q)", msg.StopReason)
	}
	r, err := ParseResponse([]byte(text.String()))
	if err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: anthropic: %w", err)
	}
	return r, nil
}

// anthropicError converts SDK errors without leaking request details (the
// SDK error string includes the request URL and raw body).
func anthropicError(err error) error {
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		return &APIError{
			Provider:   ProviderAnthropic,
			StatusCode: apierr.StatusCode,
			Type:       string(apierr.Type()),
			Message:    truncate(apierr.Message, maxErrBody),
			RequestID:  apierr.RequestID,
		}
	}
	return fmt.Errorf("llm: anthropic: %w", err)
}
