package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCheckAICommandHelper(t *testing.T) {
	if os.Getenv("CHECK_AI_SUBPROCESS") != "1" {
		return
	}
	os.Args = []string{"recovery-controller", "--check-ai"}
	main()
	os.Exit(0)
}
func TestCheckAICommandSyntheticOnlyAndSafeDiagnostic(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var models, completion atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models/image-to-text":
					models.Add(1)
					if status != 200 {
						w.WriteHeader(status)
						_, _ = w.Write([]byte("synthetic-key-secret raw-provider-body"))
						return
					}
					_, _ = w.Write([]byte(`{"data":[{"id":"vision-exact"}]}`))
				case "/v1/chat/completions":
					completion.Add(1)
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"text\":\"AB12CD\"}"}}]}`))
				default:
					t.Error("unexpected provider path:", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer provider.Close()
			cmd := exec.Command(os.Args[0], "-test.run=TestCheckAICommandHelper")
			env := []string{}
			for _, entry := range os.Environ() {
				name := strings.SplitN(entry, "=", 2)[0]
				if strings.HasPrefix(name, "ACB_") || strings.HasPrefix(name, "TELEGRAM_") || strings.HasPrefix(name, "APP_MASTER_KEY") || strings.HasPrefix(name, "AUTH_") || strings.HasPrefix(name, "NINEROUTER_") || name == "APP_ENV" || name == "AI_CAPTCHA_ENABLED" || name == "DATABASE_PATH" {
					continue
				}
				env = append(env, entry)
			}
			cmd.Env = append(env, "CHECK_AI_SUBPROCESS=1", "AI_CAPTCHA_ENABLED=true", "NINEROUTER_BASE_URL="+provider.URL, "NINEROUTER_CAPTCHA_MODEL=vision-exact", "NINEROUTER_API_KEY=synthetic-key-secret", "DATABASE_PATH=/missing/no-import.db", "ACB_PASSWORD_FILE=/missing/bank-password", "TELEGRAM_BOT_TOKEN_FILE=/missing/bot")
			output, err := cmd.CombinedOutput()
			if strings.Contains(string(output), "synthetic-key-secret") || strings.Contains(string(output), "raw-provider-body") {
				t.Fatal("preflight leaked provider secrets")
			}
			if models.Load() != 1 {
				t.Fatal("unexpected model discovery calls")
			}
			if status == 200 {
				if err != nil || completion.Load() != 1 || !strings.Contains(string(output), "PASS AI synthetic vision") {
					t.Fatalf("AI-only synthetic failed: %s %v", output, err)
				}
			} else {
				if err == nil || completion.Load() != 0 {
					t.Fatal("auth failure called completion or passed")
				}
				var diagnostic map[string]any
				if json.Unmarshal([]byte(strings.TrimSpace(string(output))), &diagnostic) != nil || diagnostic["stage"] != "MODEL_DISCOVERY" || diagnostic["code"] != "HTTP_AUTH" || diagnostic["http_status"] != float64(401) {
					t.Fatalf("missing safe typed diagnostic: %s", output)
				}
			}
		})
	}
}
