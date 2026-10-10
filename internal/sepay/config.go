package sepay

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ModeDisabled = "disabled"
	ModeObserve  = "observe"
	ModeActive   = "active"
)

// Config is an immutable, owner-verified Store receiver and Telegram source.
// QRPayload is opaque: configuration validation never rewrites bank QR data.
type Config struct {
	Mode          string    `json:"mode"`
	StoreKey      string    `json:"storeKey"`
	StoreName     string    `json:"storeName"`
	BankCode      string    `json:"bankCode"`
	BankName      string    `json:"bankName"`
	AccountNumber string    `json:"accountNumber"`
	AccountName   string    `json:"accountName"`
	QRPayload     string    `json:"qrPayload"`
	BotID         int64     `json:"botId"`
	ChatID        int64     `json:"chatId"`
	SenderBotID   int64     `json:"senderBotId"`
	TopicID       int64     `json:"topicId"`
	WebhookSecret string    `json:"webhookSecret"`
	ActivationAt  time.Time `json:"activationAt"`
}

var (
	storeKeyPattern  = regexp.MustCompile(`^[a-z0-9_-]{1,48}$`)
	accountPattern   = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	urlSchemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// ParseConfig accepts only the exact, flat configuration contract. Errors name
// trusted fields, never supplied values, secrets, QR data, or unknown keys.
// An absent configuration is disabled; a malformed supplied file is not.
func ParseConfig(raw string) (Config, error) {
	if strings.TrimSpace(raw) == "" {
		return Config{Mode: ModeDisabled}, nil
	}
	if !utf8.ValidString(raw) {
		return Config{}, fmt.Errorf("sepay config: invalid JSON")
	}
	var cfg Config
	fields := map[string]any{
		"mode": &cfg.Mode, "storeKey": &cfg.StoreKey, "storeName": &cfg.StoreName,
		"bankCode": &cfg.BankCode, "bankName": &cfg.BankName,
		"accountNumber": &cfg.AccountNumber, "accountName": &cfg.AccountName,
		"qrPayload": &cfg.QRPayload, "botId": &cfg.BotID, "chatId": &cfg.ChatID,
		"senderBotId": &cfg.SenderBotID, "topicId": &cfg.TopicID,
		"webhookSecret": &cfg.WebhookSecret, "activationAt": &cfg.ActivationAt,
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return Config{}, fmt.Errorf("sepay config: expected JSON object")
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Config{}, fmt.Errorf("sepay config: invalid JSON")
		}
		field, ok := token.(string)
		if !ok {
			return Config{}, fmt.Errorf("sepay config: invalid JSON")
		}
		target, known := fields[field]
		if !known {
			return Config{}, fmt.Errorf("sepay config: unknown field")
		}
		if seen[field] {
			return Config{}, fmt.Errorf("sepay config: duplicate %s", field)
		}
		seen[field] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return Config{}, fmt.Errorf("sepay config: invalid %s", field)
		}
		if string(value) == "null" || json.Unmarshal(value, target) != nil {
			return Config{}, fmt.Errorf("sepay config: invalid %s", field)
		}
		if field == "activationAt" {
			var timestamp string
			if json.Unmarshal(value, &timestamp) != nil || !strings.HasSuffix(timestamp, "Z") {
				return Config{}, fmt.Errorf("sepay config: activationAt must be RFC3339 UTC")
			}
		}
	}
	if last, err := decoder.Token(); err != nil || last != json.Delim('}') {
		return Config{}, fmt.Errorf("sepay config: invalid JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Config{}, fmt.Errorf("sepay config: trailing JSON")
	}
	if cfg.Mode == ModeDisabled {
		return Config{Mode: ModeDisabled}, nil
	}
	if cfg.Mode != ModeObserve && cfg.Mode != ModeActive {
		return Config{}, fmt.Errorf("sepay config: mode must be disabled, observe, or active")
	}
	for _, field := range []string{"storeKey", "storeName", "bankCode", "bankName", "accountNumber", "accountName", "qrPayload", "botId", "chatId", "senderBotId", "topicId", "webhookSecret", "activationAt"} {
		if !seen[field] {
			return Config{}, fmt.Errorf("sepay config: missing %s", field)
		}
	}
	if !storeKeyPattern.MatchString(cfg.StoreKey) {
		return Config{}, fmt.Errorf("sepay config: invalid storeKey")
	}
	for _, field := range []struct{ name, value string }{
		{"storeName", cfg.StoreName}, {"bankCode", cfg.BankCode},
		{"bankName", cfg.BankName}, {"accountName", cfg.AccountName},
	} {
		if field.value == "" || strings.TrimSpace(field.value) != field.value {
			return Config{}, fmt.Errorf("sepay config: invalid %s", field.name)
		}
	}
	if !accountPattern.MatchString(cfg.AccountNumber) {
		return Config{}, fmt.Errorf("sepay config: invalid accountNumber")
	}
	if cfg.BotID <= 0 {
		return Config{}, fmt.Errorf("sepay config: botId must be positive")
	}
	if cfg.ChatID >= 0 {
		return Config{}, fmt.Errorf("sepay config: chatId must be a negative group ID")
	}
	if cfg.SenderBotID <= 0 {
		return Config{}, fmt.Errorf("sepay config: senderBotId must be positive")
	}
	if cfg.TopicID < 0 {
		return Config{}, fmt.Errorf("sepay config: topicId must be zero or positive")
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(cfg.WebhookSecret)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != cfg.WebhookSecret {
		return Config{}, fmt.Errorf("sepay config: webhookSecret must encode 32 bytes as unpadded base64url")
	}
	payload := strings.TrimSpace(cfg.QRPayload)
	if payload == "" || len(cfg.QRPayload) > 4096 {
		return Config{}, fmt.Errorf("sepay config: qrPayload must be nonempty and at most 4096 bytes")
	}
	if urlSchemePattern.MatchString(payload) || strings.HasPrefix(payload, "/") || strings.HasPrefix(strings.ToLower(payload), "www.") {
		return Config{}, fmt.Errorf("sepay config: qrPayload must not be a URL")
	}
	if !seen["activationAt"] || cfg.ActivationAt.IsZero() {
		return Config{}, fmt.Errorf("sepay config: activationAt must be RFC3339 UTC")
	}
	return cfg, nil
}
