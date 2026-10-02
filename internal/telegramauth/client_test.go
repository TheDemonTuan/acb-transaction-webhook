package telegramauth

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestTelegramOperatorPreflightRejectsPublicChat(t *testing.T) {
	for _, kind := range []string{"private", "group", "channel"} {
		t.Run(kind, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/getChat") {
					t.Error("preflight performed an unrelated action")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": int64(123), "type": kind}})
			}))
			defer api.Close()
			client, err := NewClient("123:synthetic", ClientOptions{BaseURL: api.URL})
			if err != nil {
				t.Fatal(err)
			}
			err = client.CheckOperator(context.Background(), 123, 123)
			if (err == nil) != (kind == "private") {
				t.Fatal("private destination admission violated")
			}
		})
	}
}

func TestTelegramChallengePrivateMemoryDelivery(t *testing.T) {
	var receivedPhoto []byte
	var caption string
	var force bool
	var protected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				data, _ := io.ReadAll(part)
				switch part.FormName() {
				case "photo":
					receivedPhoto = data
					if part.FileName() != "captcha.png" {
						t.Error("unsafe attachment name")
					}
				case "caption":
					caption = string(data)
				case "protect_content":
					protected = string(data) == "true"
				case "reply_markup":
					var markup struct {
						Force bool `json:"force_reply"`
					}
					if err := json.Unmarshal(data, &markup); err != nil {
						t.Error(err)
					}
					force = markup.Force
				}
			}
		} else {
			t.Error("unexpected API method")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]int64{"message_id": 99}})
	}))
	defer server.Close()
	client, err := NewClient("123:secret", ClientOptions{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	img.Set(0, 0, color.White)
	var crop bytes.Buffer
	if err := png.Encode(&crop, img); err != nil {
		t.Fatal(err)
	}
	challenge := storage.AuthChallenge{ID: "request-safe-id", ChatID: -123, Kind: "CAPTCHA_TEXT", ExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)}
	id, err := client.SendChallenge(context.Background(), challenge, crop.Bytes())
	if err != nil || id != 99 {
		t.Fatalf("image delivery failed: %v", err)
	}
	if !bytes.Equal(receivedPhoto, crop.Bytes()) || !force || !protected || !strings.Contains(caption, "Trả lời trực tiếp ảnh này") || !strings.Contains(caption, "Hết hạn:") {
		t.Fatal("CAPTCHA reply/privacy UX incorrect")
	}
	if strings.Contains(caption, "123:secret") || strings.Contains(caption, "http") {
		t.Fatal("private attachment disclosed token/public URL")
	}
	if _, err := client.SendChallenge(context.Background(), challenge, []byte("not png")); err == nil {
		t.Fatal("invalid crop accepted")
	}
}

func TestTelegramLongPollAndDurableReject(t *testing.T) {
	ctx, s, h, _, _ := testTelegram(t)
	var requests []map[string]json.RawMessage
	var mu sync.Mutex
	var failReply bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body)
		// Wrong user is durably rejected without any command or reply side effect.
		u := command("/acb_pause")
		u.ID = 37
		u.Message.From.ID = 999
		if failReply {
			u = command("001234")
			u.ID = 38
			u.Message.ReplyTo = &Message{ID: 77}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []Update{u}})
	}))
	defer server.Close()
	h.Client.base = server.URL
	if err := h.Client.pollOnce(ctx, s, h); err != nil {
		t.Fatal(err)
	}
	state, err := s.TelegramAuthState(ctx, 42)
	if err != nil || state.NextUpdateID != 38 || state.Paused {
		t.Fatal("reject failed to persist offset without pausing")
	}
	if err := h.Client.pollOnce(ctx, s, h); err != nil {
		t.Fatal(err)
	}
	var offset, timeout, limit int
	var allowed []string
	mu.Lock()
	_ = json.Unmarshal(requests[1]["offset"], &offset)
	_ = json.Unmarshal(requests[0]["timeout"], &timeout)
	_ = json.Unmarshal(requests[0]["limit"], &limit)
	_ = json.Unmarshal(requests[0]["allowed_updates"], &allowed)
	mu.Unlock()
	if offset != 38 || timeout != 30 || limit != 100 || !reflect.DeepEqual(allowed, []string{"message", "callback_query"}) {
		t.Fatal("long-poll contract/offset incorrect")
	}
	// A transient broker failure is not a durable rejection: keep the offset.
	failing := &replyRecorder{err: context.DeadlineExceeded}
	h.ReplyBroker = failing
	mu.Lock()
	failReply = true
	mu.Unlock()
	if err := h.Client.pollOnce(ctx, s, h); err == nil {
		t.Fatal("transient bank reply failure disposed")
	}
	state, err = s.TelegramAuthState(ctx, 42)
	if err != nil || state.NextUpdateID != 38 {
		t.Fatal("transient bank failure advanced offset")
	}
}

func TestTelegramEditOutcomeClassification(t *testing.T) {
	for _, tc := range []struct{ description, want string }{
		{"Bad Request: message is not modified", ""},
		{"Bad Request: message can't be edited", "TELEGRAM_MESSAGE_UNEDITABLE"},
		{"Bad Request: message to edit not found", "TELEGRAM_MESSAGE_UNEDITABLE"},
		{"secret provider body with message can't be edited", "TELEGRAM_API_ERROR"},
	} {
		t.Run(tc.want+tc.description, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": tc.description})
			}))
			defer api.Close()
			client, err := NewClient("123:synthetic", ClientOptions{BaseURL: api.URL})
			if err != nil {
				t.Fatal(err)
			}
			err = client.EditText(context.Background(), 123, 456, "progress", nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal("unchanged progress treated as failure")
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("unexpected safe edit outcome: %v", err)
			}
		})
	}
}
