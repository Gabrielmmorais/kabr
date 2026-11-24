package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Provider names accepted in [Config.Provider].
const (
	ProviderNone      = "none"
	ProviderAnthropic = "anthropic"
	ProviderOllama    = "ollama"
)

// Defaults applied by [New].
const (
	DefaultAnthropicModel = "claude-opus-5-5"
	DefaultEffort         = "medium"
	DefaultMaxTokens      = int64(16000)
	DefaultOllamaModel    = "llama3.1"
	DefaultOllamaURL      = "http://127.0.0.1:11434"
	DefaultTimeout        = 5 * time.Minute
)

// Environment variables read by [New].
const (
	// EnvModel overrides the default model when Config.Model is empty.
	EnvModel = "KABR_LLM_MODEL"
	// EnvOllamaURL overrides the default Ollama base URL when
	// Config.BaseURL is empty.
	EnvOllamaURL = "KABR_OLLAMA_URL"
)

var (
	// ErrRefused is returned (wrapped) when the model refuses to answer.
	ErrRefused = errors.New("llm: model refused the request")
	// ErrTruncated is returned (wrapped) when the answer hit the output token
	// limit before completing.
	ErrTruncated = errors.New("llm: model output truncated by the token limit")
)

// Config selects and configures a provider.
type Config struct {
	// Provider is "anthropic", "ollama", or "none"/"" for no refinement.
	Provider string
	// Model is the model name; empty means $KABR_LLM_MODEL or the provider
	// default.
	Model string
	// Effort is the Anthropic effort level (low, medium, high, xhigh, max);
	// empty means "medium". Ignored by Ollama.
	Effort string
	// MaxTokens bounds the output; 0 means 16000 for Anthropic. For Ollama
	// it is sent as options.num_predict only when set.
	MaxTokens int64
	// BaseURL overrides the API endpoint (tests, proxies, remote Ollama).
	BaseURL string
	// HTTPClient overrides the HTTP client.
	HTTPClient *http.Client
	// Timeout bounds one request; 0 means 5 minutes.
	Timeout time.Duration
}

// Refiner proposes a refinement of a baseline plan from catalog metadata.
type Refiner interface {
	Refine(ctx context.Context, c *schema.Catalog, baseline *plan.DomainPlan) (plan.Refinement, error)
	Provider() string
	Model() string
}

// APIError is a non-success HTTP answer from a provider. It never contains
// credentials or request headers.
type APIError struct {
	Provider   string
	StatusCode int
	// Type is the provider error type when known (e.g. "rate_limit_error").
	Type string
	// Message is the provider message or a truncated response body.
	Message string
	// RequestID is the provider request ID when known.
	RequestID string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "llm: %s API error: status %d", e.Provider, e.StatusCode)
	if e.Type != "" {
		fmt.Fprintf(&b, " (%s)", e.Type)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request-id %s]", e.RequestID)
	}
	return b.String()
}

var efforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// New returns the refiner for cfg.Provider. "none" or "" returns (nil, nil):
// no refinement. An unknown provider or invalid option is an error.
func New(cfg Config) (Refiner, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.MaxTokens < 0 {
		return nil, fmt.Errorf("llm: max tokens must not be negative, got %d", cfg.MaxTokens)
	}
	if cfg.Timeout < 0 {
		return nil, fmt.Errorf("llm: timeout must not be negative, got %s", cfg.Timeout)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Model == "" {
		cfg.Model = strings.TrimSpace(os.Getenv(EnvModel))
	}
	switch provider {
	case "", ProviderNone:
		return nil, nil
	case ProviderAnthropic:
		if cfg.Model == "" {
			cfg.Model = DefaultAnthropicModel
		}
		cfg.Effort = strings.ToLower(strings.TrimSpace(cfg.Effort))
		if cfg.Effort == "" {
			cfg.Effort = DefaultEffort
		}
		if !efforts[cfg.Effort] {
			return nil, fmt.Errorf("llm: invalid effort %q (want low, medium, high, xhigh or max)", cfg.Effort)
		}
		if cfg.MaxTokens == 0 {
			cfg.MaxTokens = DefaultMaxTokens
		}
		return newAnthropic(cfg), nil
	case ProviderOllama:
		if cfg.Model == "" {
			cfg.Model = DefaultOllamaModel
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = strings.TrimSpace(os.Getenv(EnvOllamaURL))
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = DefaultOllamaURL
		}
		return newOllama(cfg)
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (want anthropic, ollama or none)", cfg.Provider)
	}
}
