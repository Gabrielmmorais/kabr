package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// maxErrBody bounds the provider text copied into an error.
const maxErrBody = 512

// maxOllamaBody bounds a successful Ollama response.
const maxOllamaBody = 16 << 20

// ollamaRefiner calls a local Ollama server's /api/chat endpoint.
type ollamaRefiner struct {
	cfg      Config
	endpoint string
	client   *http.Client
}

func newOllama(cfg Config) (*ollamaRefiner, error) {
	// Never echo the raw URL: it may carry credentials in its userinfo.
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, errors.New("llm: invalid ollama base URL (KABR_OLLAMA_URL or Config.BaseURL): cannot parse")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("llm: invalid ollama base URL %q (KABR_OLLAMA_URL or Config.BaseURL): want http(s)://host[:port]", u.Redacted())
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &ollamaRefiner{cfg: cfg, endpoint: u.String() + "/api/chat", client: client}, nil
}

func (o *ollamaRefiner) Provider() string { return ProviderOllama }
func (o *ollamaRefiner) Model() string    { return o.cfg.Model }

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Format   map[string]any  `json:"format"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options"`
}

type ollamaResponse struct {
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason"`
}

func (o *ollamaRefiner) request(p Prompt) ollamaRequest {
	opts := map[string]any{"temperature": 0}
	if o.cfg.MaxTokens > 0 {
		opts["num_predict"] = o.cfg.MaxTokens
	}
	return ollamaRequest{
		Model: o.cfg.Model,
		Messages: []ollamaMessage{
			{Role: "system", Content: p.System},
			{Role: "user", Content: p.User},
		},
		Format:  ResponseSchema(),
		Stream:  false,
		Options: opts,
	}
}

func (o *ollamaRefiner) Refine(ctx context.Context, c *schema.Catalog, baseline *plan.DomainPlan) (plan.Refinement, error) {
	p, err := BuildPrompt(c, baseline)
	if err != nil {
		return plan.Refinement{}, err
	}
	body, err := json.Marshal(o.request(p))
	if err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: ollama: encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint, bytes.NewReader(body))
	if err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: ollama: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req) // #nosec G704 -- endpoint is operator configuration
	if err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody+1))
		return plan.Refinement{}, &APIError{
			Provider:   ProviderOllama,
			StatusCode: resp.StatusCode,
			Message:    truncate(strings.TrimSpace(string(raw)), maxErrBody),
		}
	}
	var out ollamaResponse
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxOllamaBody)).Decode(&out); err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: ollama: decode response: %w", err)
	}
	if out.DoneReason == "length" {
		return plan.Refinement{}, fmt.Errorf("%w (ollama done_reason length)", ErrTruncated)
	}
	r, err := ParseResponse([]byte(out.Message.Content))
	if err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: ollama: %w", err)
	}
	return r, nil
}

// truncate shortens s to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}
