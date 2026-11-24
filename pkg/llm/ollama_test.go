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
	"testing"
	"time"
)

func newTestOllama(t *testing.T, h http.HandlerFunc, maxTokens int64) Refiner {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv(EnvModel, "")
	r, err := New(Config{Provider: "ollama", BaseURL: srv.URL + "/", HTTPClient: srv.Client(), MaxTokens: maxTokens})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOllamaRefineSuccess(t *testing.T) {
	var got map[string]any
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": DefaultOllamaModel, "done": true, "done_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": canned},
		})
	}
	r := newTestOllama(t, h, 0)
	c := pagila(t)
	base := baselineFor(t, c)
	ref, err := r.Refine(context.Background(), c, base)
	if err != nil {
		t.Fatalf("Refine: %v", err)
	}
	want, _ := ParseResponse([]byte(canned))
	if !reflect.DeepEqual(ref, want) {
		t.Fatalf("got %+v", ref)
	}
	p, _ := BuildPrompt(c, base)
	wantBody := roundTrip(t, map[string]any{
		"model": DefaultOllamaModel,
		"messages": []any{
			map[string]any{"role": "system", "content": p.System},
			map[string]any{"role": "user", "content": p.User},
		},
		"format":  ResponseSchema(),
		"stream":  false,
		"options": map[string]any{"temperature": 0},
	})
	if !reflect.DeepEqual(any(got), wantBody) {
		t.Fatalf("request body mismatch:\n%v", got)
	}
}

func TestOllamaNumPredict(t *testing.T) {
	var got map[string]any
	h := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"done":true,"message":{"role":"assistant","content":"{}"}}`)
	}
	r := newTestOllama(t, h, 4096)
	c := pagila(t)
	if _, err := r.Refine(context.Background(), c, baselineFor(t, c)); err != nil {
		t.Fatal(err)
	}
	if opts := got["options"].(map[string]any); opts["num_predict"] != float64(4096) {
		t.Fatalf("options = %v", opts)
	}
}

func TestOllamaRefineFailures(t *testing.T) {
	long := strings.Repeat("x", 2000)
	tests := []struct {
		name    string
		status  int
		body    string
		wantIs  error
		wantMsg string
		check   func(t *testing.T, err error)
	}{
		{name: "length", status: 200, body: `{"done":true,"done_reason":"length","message":{"role":"assistant","content":"{\"renames\":["}}`, wantIs: ErrTruncated},
		{
			name: "server error", status: 500, body: `{"error":"model not found"}` + long, wantMsg: "model not found",
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 || apiErr.Provider != "ollama" {
					t.Fatalf("err = %#v", err)
				}
				if len(apiErr.Message) > maxErrBody+3 {
					t.Fatalf("message not truncated: %d bytes", len(apiErr.Message))
				}
			},
		},
		{name: "bad envelope", status: 200, body: `nope`, wantMsg: "decode response"},
		{name: "bad answer", status: 200, body: `{"done":true,"message":{"role":"assistant","content":"{\"x\":1}"}}`, wantMsg: "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}
			r := newTestOllama(t, h, 0)
			c := pagila(t)
			_, err := r.Refine(context.Background(), c, baselineFor(t, c))
			if err == nil {
				t.Fatal("want error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("err = %v, want Is %v", err, tt.wantIs)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want containing %q", err, tt.wantMsg)
			}
			if tt.check != nil {
				tt.check(t, err)
			}
		})
	}
}

func TestOllamaRespectsContext(t *testing.T) {
	release := make(chan struct{})
	h := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	r := newTestOllama(t, h, 0)
	defer close(release)
	c := pagila(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Refine(ctx, c, baselineFor(t, c)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}
