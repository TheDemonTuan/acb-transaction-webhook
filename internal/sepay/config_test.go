package sepay

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testConfigFields() map[string]any {
	return map[string]any{
		"mode": ModeActive, "storeKey": "test-store_1", "storeName": "Test Store",
		"bankCode": "TESTBANK", "bankName": "Fixture Bank",
		"accountNumber": "VA012345", "accountName": "TEST RECEIVER",
		"qrPayload": "0002010102116304ABCD",
		"botId":     int64(900001), "chatId": int64(-100900003),
		"senderBotId": int64(900002), "topicId": int64(0),
		"webhookSecret": base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901")),
		"activationAt":  "2026-10-10T00:00:00Z",
	}
}

func configJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseConfigDisabled(t *testing.T) {
	for _, raw := range []string{"", " \r\n", `{"mode":"disabled"}`} {
		cfg, err := ParseConfig(raw)
		if err != nil || cfg != (Config{Mode: ModeDisabled}) {
			t.Fatalf("absent/disabled config was not disabled: %v", err)
		}
	}
	fields := testConfigFields()
	fields["mode"] = ModeDisabled
	cfg, err := ParseConfig(configJSON(t, fields))
	if err != nil || cfg != (Config{Mode: ModeDisabled}) {
		t.Fatal("disabled config retained receiver or credentials")
	}
}

func TestParseConfigEnabledPreservesVerifiedValues(t *testing.T) {
	for _, mode := range []string{ModeObserve, ModeActive} {
		t.Run(mode, func(t *testing.T) {
			fields := testConfigFields()
			fields["mode"] = mode
			fields["botId"] = int64(9007199254740993)
			fields["chatId"] = int64(-9007199254740993)
			fields["topicId"] = int64(42)
			cfg, err := ParseConfig(configJSON(t, fields))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mode != mode || cfg.BotID != 9007199254740993 || cfg.ChatID != -9007199254740993 || cfg.TopicID != 42 {
				t.Fatal("mode or int64 identity changed")
			}
			if cfg.QRPayload != fields["qrPayload"] || cfg.AccountNumber != "VA012345" || cfg.BankName != "Fixture Bank" || cfg.BankCode != "TESTBANK" {
				t.Fatal("verified receiver values changed")
			}
			if !cfg.ActivationAt.Equal(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)) {
				t.Fatal("activation boundary changed")
			}
		})
	}
}

func TestParseConfigRequiresEveryEnabledField(t *testing.T) {
	for field := range testConfigFields() {
		t.Run(field, func(t *testing.T) {
			fields := testConfigFields()
			delete(fields, field)
			if _, err := ParseConfig(configJSON(t, fields)); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("missing field not rejected with safe field name: %v", err)
			}
		})
	}
}

func TestParseConfigRejectsInvalidFields(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{
		{"mode", "ACTIVE"}, {"mode", ""}, {"storeKey", "Store"},
		{"storeKey", strings.Repeat("a", 49)}, {"storeKey", "store/name"},
		{"storeName", ""}, {"storeName", " Store"}, {"bankCode", ""},
		{"bankName", ""}, {"accountName", ""}, {"accountNumber", ""},
		{"accountNumber", "VA 123"}, {"accountNumber", "123-456"},
		{"botId", 0}, {"botId", -1}, {"botId", 1.5}, {"botId", "900001"},
		{"chatId", 0}, {"chatId", 100}, {"senderBotId", 0}, {"topicId", -1},
		{"webhookSecret", "secret"}, {"webhookSecret", strings.Repeat("A", 44)},
		{"webhookSecret", base64.URLEncoding.EncodeToString(make([]byte, 32))},
		{"qrPayload", ""}, {"qrPayload", " \n"}, {"qrPayload", strings.Repeat("a", 4097)},
		{"qrPayload", "https://payments.example/pay"}, {"qrPayload", "//payments.example/pay"},
		{"qrPayload", "www.payments.example/pay"}, {"qrPayload", "javascript:alert(1)"},
		{"qrPayload", "https://payments.example/%ZZ"}, {"qrPayload", "/pay"},
		{"qrPayload", strings.Repeat("é", 2049)},
		{"activationAt", "2026-10-10"}, {"activationAt", "2026-10-10T07:00:00+07:00"},
		{"activationAt", "2026-10-10T00:00:00+00:00"}, {"activationAt", "0001-01-01T00:00:00Z"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			fields := testConfigFields()
			fields[tc.field] = tc.value
			if _, err := ParseConfig(configJSON(t, fields)); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("invalid field not rejected with safe field name: %v", err)
			}
		})
	}
	for field := range testConfigFields() {
		fields := testConfigFields()
		fields[field] = nil
		if _, err := ParseConfig(configJSON(t, fields)); err == nil {
			t.Fatalf("null %s accepted", field)
		}
	}
}

func TestParseConfigRejectsAmbiguousJSONWithoutLeakingValues(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `[]`, `"disabled"`, `{"mode":"disabled"} {"mode":"active"}`,
		`{"mode":"disabled","mode":"active"}`, `{"mode":"disabled","Mode":"active"}`,
		`{"mode":"disabled","secret-value-is-an-unknown-key":"private-value"}`,
		`{"mode":"private-mode-value"}`, `{"mode":"disabled",}`,
		`{"mode":"active","botId":9223372036854775808}`, `{"mode":"disabled","botId":1e3}`,
	} {
		_, err := ParseConfig(raw)
		if err == nil {
			t.Fatal("ambiguous or malformed JSON accepted")
		}
		for _, private := range []string{"secret-value", "private-value", "private-mode-value", "9223372036854775808"} {
			if strings.Contains(err.Error(), private) {
				t.Fatal("error leaked supplied content")
			}
		}
	}
}

func TestParseConfigPreservesOpaqueQRAtByteLimit(t *testing.T) {
	fields := testConfigFields()
	payload := strings.Repeat("é", 2048)
	fields["qrPayload"] = payload
	cfg, err := ParseConfig(configJSON(t, fields))
	if err != nil || cfg.QRPayload != payload {
		t.Fatalf("4096-byte opaque payload was rejected or rewritten: %v", err)
	}
}
