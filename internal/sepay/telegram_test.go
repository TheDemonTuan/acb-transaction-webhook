package sepay

import (
	"bytes"
	"encoding/json"
	"testing"
)

func protocolConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := ParseConfig(configJSON(t, testConfigFields()))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func telegramFixture(t *testing.T, cfg Config, updateID, messageID int64, text string) map[string]any {
	t.Helper()
	return map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": messageID, "date": int64(1791603000),
			"from": map[string]any{"id": cfg.SenderBotID, "is_bot": true, "first_name": "SePay Bot"},
			"chat": map[string]any{"id": cfg.ChatID, "type": "supergroup"},
			"text": text,
		},
	}
}

func decodeFixture(t *testing.T, fields map[string]any) TelegramUpdate {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	update, err := DecodeTelegramUpdate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return update
}

func TestDecodeTelegramUpdatePreservesOriginalInt64Envelope(t *testing.T) {
	raw := []byte(" { \"update_id\":9007199254740993,\"message\":{\"message_id\":9007199254740995,\"date\":1791603000,\"from\":{\"id\":9007199254740997,\"is_bot\":true},\"chat\":{\"id\":-9007199254740999},\"text\":\"text\"}} \n")
	update, err := DecodeTelegramUpdate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if update.UpdateID != 9007199254740993 || update.Message.MessageID != 9007199254740995 || update.Message.From.ID != 9007199254740997 || update.Message.Chat.ID != -9007199254740999 || !bytes.Equal(update.RawPayload, raw) {
		t.Fatal("integer identity or original envelope changed")
	}
}

func TestDecodeTelegramUpdateRejectsInvalidIntegerEnvelopes(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{"update_id":null}`, `{"update_id":1.5}`,
		`{"update_id":1e3}`, `{"update_id":"1"}`, `{"update_id":9223372036854775808}`,
		`{"update_id":1,"message":{"message_id":1.5}}`,
		`{"update_id":1,"message":{"from":{"id":1e3}}}`,
		`{"update_id":1,"message":{"chat":{"id":-1.5}}}`,
		`{"update_id":1} {"update_id":2}`,
	} {
		if _, err := DecodeTelegramUpdate([]byte(raw)); err == nil {
			t.Fatalf("invalid envelope accepted: %s", raw)
		}
	}
	update, err := DecodeTelegramUpdate([]byte(`{}`))
	if err != nil || !update.unsupported {
		t.Fatal("unsupported object must decode for an ignored acknowledgement")
	}
}

func TestTelegramTrustAllowlist(t *testing.T) {
	cfg := protocolConfig(t)
	cases := []struct {
		name string
		edit func(map[string]any, map[string]any)
	}{
		{"wrong chat", func(_ map[string]any, m map[string]any) { m["chat"].(map[string]any)["id"] = int64(-22) }},
		{"wrong sender", func(_ map[string]any, m map[string]any) { m["from"].(map[string]any)["id"] = int64(42) }},
		{"human named SePay", func(_ map[string]any, m map[string]any) { m["from"].(map[string]any)["is_bot"] = false }},
		{"missing sender", func(_ map[string]any, m map[string]any) { delete(m, "from") }},
		{"wrong topic", func(_ map[string]any, m map[string]any) { m["message_thread_id"] = int64(42) }},
		{"sender chat", func(_ map[string]any, m map[string]any) { m["sender_chat"] = map[string]any{"id": cfg.ChatID} }},
		{"forward origin", func(_ map[string]any, m map[string]any) { m["forward_origin"] = map[string]any{"type": "user"} }},
		{"legacy forward", func(_ map[string]any, m map[string]any) { m["forward_from"] = map[string]any{"id": cfg.SenderBotID} }},
		{"via bot", func(_ map[string]any, m map[string]any) { m["via_bot"] = map[string]any{"id": cfg.SenderBotID} }},
		{"reply copy", func(_ map[string]any, m map[string]any) { m["reply_to_message"] = map[string]any{"message_id": 2} }},
		{"service message", func(_ map[string]any, m map[string]any) { m["new_chat_members"] = []any{} }},
		{"caption not text", func(_ map[string]any, m map[string]any) { m["caption"] = m["text"]; delete(m, "text") }},
		{"missing text", func(_ map[string]any, m map[string]any) { delete(m, "text") }},
		{"edited as original", func(_ map[string]any, m map[string]any) { m["edit_date"] = int64(1791603001) }},
		{"two message kinds", func(u map[string]any, m map[string]any) { u["edited_message"] = m }},
		{"extra update kind", func(u map[string]any, _ map[string]any) { u["callback_query"] = map[string]any{"data": "private"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := telegramFixture(t, cfg, 1, 1, fixtureNotification)
			tc.edit(fields, fields["message"].(map[string]any))
			if message, _ := decodeFixture(t, fields).trustedMessage(cfg); message != nil {
				t.Fatal("untrusted message admitted")
			}
		})
	}
	fields := telegramFixture(t, cfg, 1, 1, fixtureNotification)
	fields["message"].(map[string]any)["entities"] = []any{map[string]any{"type": "bold", "offset": 0, "length": 14}}
	if message, edited := decodeFixture(t, fields).trustedMessage(cfg); message == nil || edited || message.Text != fixtureNotification {
		t.Fatal("original text was rejected or transformed")
	}
	fields["edited_message"] = fields["message"]
	delete(fields, "message")
	if message, edited := decodeFixture(t, fields).trustedMessage(cfg); message == nil || !edited {
		t.Fatal("trusted edit did not enter review path")
	}
	cfg.TopicID = 42
	fields = telegramFixture(t, cfg, 1, 1, fixtureNotification)
	if message, _ := decodeFixture(t, fields).trustedMessage(cfg); message != nil {
		t.Fatal("topic configuration accepted unthreaded message")
	}
	fields["message"].(map[string]any)["message_thread_id"] = int64(42)
	if message, _ := decodeFixture(t, fields).trustedMessage(cfg); message == nil {
		t.Fatal("correct topic rejected")
	}
}
