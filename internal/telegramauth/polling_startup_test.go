package telegramauth

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimePollingRegistersCommandsOnlyAfterConfig(t *testing.T) {
	// Cancellation is driven by the first poll; this is only a deadlock watchdog,
	// not a one-second performance requirement for race-instrumented CI.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		calls = append(calls, method)
		mu.Unlock()
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 42, "is_bot": true}
		case "getWebhookInfo":
			result = map[string]any{"url": ""}
		case "setMyCommands":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid registration")
			}
			scope, _ := body["scope"].(map[string]any)
			if scope["chat_id"] != float64(123) {
				t.Error("wrong command scope")
			}
		case "getUpdates":
			cancel()
			result = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer api.Close()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, err := NewClient("123:synthetic", ClientOptions{BaseURL: api.URL, HTTPClient: api.Client()})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(HandlerOptions{Store: store, Client: client, ChatID: 123, UserID: 456, PublicOrigin: "https://bank.example"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.RunPolling(ctx, store, h, 123)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, ",") != "getMe,getWebhookInfo,setMyCommands,getUpdates" {
		t.Fatalf("unsafe startup order: %v", calls)
	}
}
