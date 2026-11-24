package llm

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name         string
		cfg          Config
		env          map[string]string
		wantNil      bool
		wantErr      string
		noEcho       string
		wantProvider string
		wantModel    string
		check        func(t *testing.T, r Refiner)
	}{
		{name: "empty provider", cfg: Config{}, wantNil: true},
		{name: "none", cfg: Config{Provider: "none"}, wantNil: true},
		{name: "none ignores env", cfg: Config{Provider: " NONE "}, env: map[string]string{EnvModel: "x"}, wantNil: true},
		{name: "unknown", cfg: Config{Provider: "openai"}, wantErr: "unknown provider"},
		{name: "negative tokens", cfg: Config{Provider: "anthropic", MaxTokens: -1}, wantErr: "max tokens"},
		{name: "negative timeout", cfg: Config{Provider: "ollama", Timeout: -time.Second}, wantErr: "timeout"},
		{name: "bad effort", cfg: Config{Provider: "anthropic", Effort: "extreme"}, wantErr: "invalid effort"},
		{name: "bad ollama url", cfg: Config{Provider: "ollama", BaseURL: "ftp://x"}, wantErr: "invalid ollama base URL"},
		{name: "unparseable ollama url", cfg: Config{Provider: "ollama", BaseURL: "http://user:hunter2@[::1"}, wantErr: "cannot parse", noEcho: "hunter2"},
		{name: "ollama url userinfo redacted", cfg: Config{Provider: "ollama", BaseURL: "ftp://user:hunter2@host"}, wantErr: "xxxxx", noEcho: "hunter2"},
		{name: "ollama env url userinfo redacted", cfg: Config{Provider: "ollama"}, env: map[string]string{EnvOllamaURL: "ftp://user:hunter2@host"}, wantErr: "invalid ollama base URL", noEcho: "hunter2"},
		{
			name: "anthropic defaults", cfg: Config{Provider: "Anthropic"},
			wantProvider: "anthropic", wantModel: DefaultAnthropicModel,
			check: func(t *testing.T, r Refiner) {
				a := r.(*anthropicRefiner)
				if a.cfg.Effort != "medium" || a.cfg.MaxTokens != 16000 || a.cfg.Timeout != 5*time.Minute {
					t.Errorf("defaults = %+v", a.cfg)
				}
			},
		},
		{
			name: "anthropic env model", cfg: Config{Provider: "anthropic", Effort: "HIGH", MaxTokens: 2000},
			env:          map[string]string{EnvModel: "claude-test-model"},
			wantProvider: "anthropic", wantModel: "claude-test-model",
			check: func(t *testing.T, r Refiner) {
				a := r.(*anthropicRefiner)
				if a.cfg.Effort != "high" || a.cfg.MaxTokens != 2000 {
					t.Errorf("cfg = %+v", a.cfg)
				}
			},
		},
		{
			name: "explicit model beats env", cfg: Config{Provider: "anthropic", Model: "explicit"},
			env:          map[string]string{EnvModel: "from-env"},
			wantProvider: "anthropic", wantModel: "explicit",
		},
		{
			name: "ollama defaults", cfg: Config{Provider: "ollama"},
			wantProvider: "ollama", wantModel: DefaultOllamaModel,
			check: func(t *testing.T, r Refiner) {
				o := r.(*ollamaRefiner)
				if o.endpoint != DefaultOllamaURL+"/api/chat" || o.cfg.MaxTokens != 0 {
					t.Errorf("ollama = %+v", o)
				}
			},
		},
		{
			name: "ollama env", cfg: Config{Provider: "ollama"},
			env:          map[string]string{EnvOllamaURL: "http://gpu-box:11434/", EnvModel: "qwen3"},
			wantProvider: "ollama", wantModel: "qwen3",
			check: func(t *testing.T, r Refiner) {
				if got := r.(*ollamaRefiner).endpoint; got != "http://gpu-box:11434/api/chat" {
					t.Errorf("endpoint = %q", got)
				}
			},
		},
		{
			name: "ollama explicit url and client", cfg: Config{Provider: "ollama", BaseURL: "https://ollama.example", HTTPClient: http.DefaultClient},
			env:          map[string]string{EnvOllamaURL: "http://ignored:1"},
			wantProvider: "ollama", wantModel: DefaultOllamaModel,
			check: func(t *testing.T, r Refiner) {
				o := r.(*ollamaRefiner)
				if o.endpoint != "https://ollama.example/api/chat" || o.client != http.DefaultClient {
					t.Errorf("ollama = %+v", o)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvModel, "")
			t.Setenv(EnvOllamaURL, "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			r, err := New(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if tt.noEcho != "" && strings.Contains(err.Error(), tt.noEcho) {
					t.Fatalf("err leaks %q: %v", tt.noEcho, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if tt.wantNil {
				if r != nil {
					t.Fatalf("got %T, want nil", r)
				}
				return
			}
			if r.Provider() != tt.wantProvider || r.Model() != tt.wantModel {
				t.Fatalf("got %s/%s, want %s/%s", r.Provider(), r.Model(), tt.wantProvider, tt.wantModel)
			}
			if tt.check != nil {
				tt.check(t, r)
			}
		})
	}
}

func TestAPIErrorString(t *testing.T) {
	e := &APIError{Provider: "anthropic", StatusCode: 429, Type: "rate_limit_error", Message: "slow down", RequestID: "req_1"}
	want := "llm: anthropic API error: status 429 (rate_limit_error): slow down [request-id req_1]"
	if e.Error() != want {
		t.Fatalf("got %q", e.Error())
	}
	if got := truncate(strings.Repeat("é", 300), 11); got != "ééééé..." {
		t.Fatalf("truncate = %q", got)
	}
}
