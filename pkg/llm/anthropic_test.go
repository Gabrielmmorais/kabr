package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// fakeMessages mimics POST /v1/messages and records the last request body.
type fakeMessages struct {
	t      *testing.T
	status int
	body   string

	mu      sync.Mutex
	got     []byte
	headers http.Header
}

func (f *fakeMessages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read body: %v", err)
	}
	f.mu.Lock()
	f.got, f.headers = b, r.Header.Clone()
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-should-retry", "false") // keep error tests fast
	w.Header().Set("request-id", "req_test")
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func messageJSON(t *testing.T, stopReason string, stopDetails any, content ...map[string]any) string {
	t.Helper()
	if content == nil {
		content = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": DefaultAnthropicModel,
		"content": content, "stop_reason": stopReason, "stop_details": stopDetails,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestAnthropic(t *testing.T, f *fakeMessages) Refiner {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_API_KEY", "test")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_PROFILE", "")
	t.Setenv(EnvModel, "")
	r, err := New(Config{Provider: "anthropic", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAnthropicRefineSuccess(t *testing.T) {
	f := &fakeMessages{t: t, status: http.StatusOK}
	f.body = messageJSON(t, "end_turn", nil,
		map[string]any{"type": "thinking", "thinking": `{"renames":"not the answer"}`, "signature": "sig"},
		map[string]any{"type": "text", "text": canned},
	)
	r := newTestAnthropic(t, f)
	c := pagila(t)
	base := baselineFor(t, c)

	got, err := r.Refine(context.Background(), c, base)
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	want, err := ParseResponse([]byte(canned))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.SensitiveColumns[0].Source != plan.SourceLLM {
		t.Fatalf("source = %q", got.SensitiveColumns[0].Source)
	}

	// Request body.
	raw := string(f.got)
	for _, s := range []string{`"type":"json_schema"`, `"effort":"medium"`, `"output_config":{`} {
		if !strings.Contains(raw, s) {
			t.Errorf("request body lacks %s", s)
		}
	}
	var body map[string]any
	if err = json.Unmarshal(f.got, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != DefaultAnthropicModel || body["max_tokens"] != float64(16000) {
		t.Errorf("model/max_tokens = %v/%v", body["model"], body["max_tokens"])
	}
	if _, ok := body["thinking"]; ok {
		t.Error("thinking must be left unset")
	}
	oc := body["output_config"].(map[string]any)
	format := oc["format"].(map[string]any)
	if oc["effort"] != "medium" || format["type"] != "json_schema" {
		t.Errorf("output_config = %v", oc)
	}
	wantSchema := roundTrip(t, ResponseSchema())
	if !reflect.DeepEqual(format["schema"], wantSchema) {
		t.Errorf("format.schema = %v", format["schema"])
	}
	p, err := BuildPrompt(c, base)
	if err != nil {
		t.Fatal(err)
	}
	system := body["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != p.System {
		t.Errorf("system = %v", system)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	m := msgs[0].(map[string]any)
	content := m["content"].([]any)
	if m["role"] != "user" || len(content) != 1 || content[0].(map[string]any)["text"] != p.User {
		t.Errorf("user message = %v", m)
	}
	// The SDK attaches the credential itself (v2.14 uses Authorization: Bearer).
	if f.headers.Get("X-Api-Key") != "test" && f.headers.Get("Authorization") != "Bearer test" {
		t.Errorf("API key header not sent by the SDK: %v", f.headers)
	}
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnthropicRefineFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       func(t *testing.T) string
		wantIs     error
		wantStatus int
		wantMsg    string
	}{
		{
			name: "refusal", status: http.StatusOK,
			body: func(t *testing.T) string {
				return messageJSON(t, "refusal", map[string]any{"type": "refusal", "category": "cyber", "explanation": nil},
					map[string]any{"type": "text", "text": canned})
			},
			wantIs: ErrRefused, wantMsg: "category cyber",
		},
		{
			name: "refusal without details", status: http.StatusOK,
			body:   func(t *testing.T) string { return messageJSON(t, "refusal", nil) },
			wantIs: ErrRefused,
		},
		{
			name: "max tokens", status: http.StatusOK,
			body: func(t *testing.T) string {
				return messageJSON(t, "max_tokens", nil, map[string]any{"type": "text", "text": `{"renames":[`})
			},
			wantIs: ErrTruncated, wantMsg: "max_tokens",
		},
		{
			name: "no text", status: http.StatusOK,
			body: func(t *testing.T) string {
				return messageJSON(t, "end_turn", nil, map[string]any{"type": "thinking", "thinking": "x", "signature": "s"})
			},
			wantMsg: "no text",
		},
		{
			name: "invalid answer", status: http.StatusOK,
			body: func(t *testing.T) string {
				return messageJSON(t, "end_turn", nil, map[string]any{"type": "text", "text": `{"bogus":true}`})
			},
			wantMsg: "unknown field",
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests,
			body: func(*testing.T) string {
				return `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req_test"}`
			},
			wantStatus: 429, wantMsg: "rate_limit_error",
		},
		{
			name: "server error", status: http.StatusInternalServerError,
			body: func(*testing.T) string {
				return `{"type":"error","error":{"type":"api_error","message":"boom"}}`
			},
			wantStatus: 500, wantMsg: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMessages{t: t, status: tt.status, body: tt.body(t)}
			r := newTestAnthropic(t, f)
			c := pagila(t)
			_, err := r.Refine(context.Background(), c, baselineFor(t, c))
			if err == nil {
				t.Fatal("want error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("err = %v, want Is %v", err, tt.wantIs)
			}
			if tt.wantStatus != 0 {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.wantStatus || apiErr.Provider != "anthropic" {
					t.Errorf("err = %#v, want APIError %d", err, tt.wantStatus)
				}
				// Never the SDK string: it carries the URL and raw body.
				if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "/v1/messages") {
					t.Errorf("error leaks request details: %v", err)
				}
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want containing %q", err, tt.wantMsg)
			}
		})
	}
}

func TestAnthropicContextCanceled(t *testing.T) {
	f := &fakeMessages{t: t, status: http.StatusOK, body: messageJSON(t, "end_turn", nil)}
	r := newTestAnthropic(t, f)
	c := pagila(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Refine(ctx, c, baselineFor(t, c)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
