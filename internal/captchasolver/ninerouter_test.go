package captchasolver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCaptchaSolverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name, content               string
		status                      int
		oversized, refusal, timeout bool
	}{{name: "valid", content: `{"text":"Ab12CD"}`, status: 200}, {name: "empty", content: `{"text":""}`, status: 200}, {name: "prose", content: `Answer: {"text":"AB12CD"}`, status: 200}, {name: "markdown", content: "```json\n{\"text\":\"AB12CD\"}\n```", status: 200}, {name: "additional field", content: `{"text":"AB12CD","confidence":1}`, status: 200}, {name: "duplicate key", content: `{"text":"AB12CD","text":"ABCDEF"}`, status: 200}, {name: "trailing object", content: `{"text":"AB12CD"}{}`, status: 200}, {name: "unicode", content: `{"text":"ＡＢ１２"}`, status: 200}, {name: "malformed", content: `{"text":`, status: 200}, {name: "refusal", content: `{"text":"AB12CD"}`, status: 200, refusal: true}, {name: "429", status: 429}, {name: "oversized", status: 200, oversized: true}, {name: "timeout", status: 200, timeout: true}} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var wire struct {
					Model    string `json:"model"`
					Stream   bool   `json:"stream"`
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if json.Unmarshal(body, &wire) != nil || wire.Model != "vision-exact" || wire.Stream || len(wire.Messages) != 2 || !strings.Contains(string(wire.Messages[1].Content), "data:image/png;base64,") {
					t.Error("vision wire contract violated")
				}
				if strings.Contains(string(body), "password") || strings.Contains(string(body), "otp") {
					t.Error("unexpected banking context")
				}
				if tc.timeout {
					cancel(context.DeadlineExceeded)
					return
				}
				w.WriteHeader(tc.status)
				if tc.oversized {
					_, _ = io.WriteString(w, strings.Repeat("x", 16<<10+1))
					return
				}
				message := map[string]any{"content": tc.content}
				if tc.refusal {
					message["refusal"] = "cannot process"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message}}})
			}))
			defer api.Close()
			solver, err := NewNineRouter(Config{BaseURL: api.URL + "/v1", Model: "vision-exact", APIKey: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			answer, err := solver.Solve(ctx, SyntheticPNG())
			if tc.name == "valid" {
				if err != nil || answer != "Ab12CD" {
					t.Fatal("valid transcription rejected")
				}
			} else if !errors.Is(err, ErrUnavailable) || answer != "" {
				t.Fatalf("unsafe result accepted err=%v", err)
			}
			if requests.Load() != 1 {
				t.Fatalf("AI retried requests=%d", requests.Load())
			}
		})
	}
}
func TestNineRouterExactModelDiscovery(t *testing.T) {
	var discoveries atomic.Int32
	var solves atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models/image-to-text":
			w.WriteHeader(404)
		case "/v1/models":
			discoveries.Add(1)
			_, _ = io.WriteString(w, `{"data":[{"id":"vision-exact","owned_by":"provider"}]}`)
		case "/v1/chat/completions":
			solves.Add(1)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"text\":\"AB12CD\"}"}}]}`)
		default:
			t.Error("unexpected path")
		}
	}))
	defer api.Close()
	n, err := NewNineRouter(Config{BaseURL: api.URL, Model: "vision-exact", APIKey: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if discoveries.Load() != 1 || solves.Load() != 1 {
		t.Fatal("discovery fallback or synthetic vision missing")
	}
}
func TestNineRouterRejectsUnsafeEndpointAndRedirect(t *testing.T) {
	for _, base := range []string{"https://user:secret@example/v1", "https://example/v1?secret", "https://example/v1#secret", "https://example/v1/v1", "http://public.example/v1"} {
		if _, err := NewNineRouter(Config{BaseURL: base, Model: "vision", APIKey: "synthetic", Production: true}); err == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	n, err := NewNineRouter(Config{BaseURL: origin.URL, Model: "vision", APIKey: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Solve(context.Background(), SyntheticPNG()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if leaked.Load() != 0 {
		t.Fatal("API key followed redirect")
	}
}
