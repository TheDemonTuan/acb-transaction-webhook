package sepay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// BootstrapConfig consults encrypted storage before the legacy file. Managed
// data is checked at startup, but never becomes a stale file fallback.
func BootstrapConfig(ctx context.Context, store *storage.Store, legacy string) (Config, error) {
	snapshot, err := NewService(Config{Mode: ModeDisabled}, store, nil).Snapshot(ctx)
	if err != nil {
		return Config{}, err
	}
	if snapshot.revision > 0 {
		return Config{Mode: ModeDisabled}, nil
	}
	return ParseConfig(legacy)
}

// AdminFields deliberately excludes every secret and encodes Telegram IDs as
// decimal strings, including values beyond JavaScript's exact integer range.
type AdminFields struct {
	Mode                      string `json:"mode"`
	StoreKey                  string `json:"storeKey"`
	StoreName                 string `json:"storeName"`
	BankCode                  string `json:"bankCode"`
	BankName                  string `json:"bankName"`
	AccountNumber             string `json:"accountNumber"`
	NotificationAccountNumber string `json:"notificationAccountNumber"`
	AccountName               string `json:"accountName"`
	QRPayload                 string `json:"qrPayload"`
	BotID                     string `json:"botId"`
	ChatID                    string `json:"chatId"`
	SenderBotID               string `json:"senderBotId"`
	TopicID                   string `json:"topicId"`
	ActivationAt              string `json:"activationAt"`
	ReceiverVerified          bool   `json:"receiverVerified"`
	SourceSeparated           bool   `json:"sourceSeparated"`
}

type managedFields struct {
	Fields        AdminFields `json:"fields"`
	WebhookSecret string      `json:"webhookSecret"`
}

type FieldError struct{ Field string }

func (e *FieldError) Error() string { return "invalid SePay configuration field: " + e.Field }

// Snapshot is request-scoped. The webhook uses this same value for secret
// authentication, source allowlisting, parser settings and the commit revision.
func (s *Service) Snapshot(ctx context.Context) (*Service, error) {
	if s == nil {
		return NewService(Config{Mode: ModeDisabled}, nil, nil), nil
	}
	if s.pinned || s.store == nil {
		return s, nil
	}
	record, found, err := s.store.SePayManagedConfig(ctx)
	if err != nil {
		return nil, err
	}
	copy := *s
	copy.pinned = true
	copy.revision = 0
	if !found {
		return &copy, nil
	}
	var saved managedFields
	if json.Unmarshal(record.ConfigJSON, &saved) != nil {
		return nil, storage.ErrSePayConfigUnavailable
	}
	cfg, err := adminRuntime(saved.Fields, saved.WebhookSecret)
	if err != nil {
		return nil, storage.ErrSePayConfigUnavailable
	}
	copy.cfg, copy.revision, copy.token, copy.fields = cfg, record.Revision, record.BotToken, &saved.Fields
	return &copy, nil
}

func fieldsFromConfig(cfg Config) AdminFields {
	f := AdminFields{Mode: cfg.Mode, StoreKey: cfg.StoreKey, StoreName: cfg.StoreName, BankCode: cfg.BankCode, BankName: cfg.BankName, AccountNumber: cfg.AccountNumber, NotificationAccountNumber: cfg.NotificationAccountNumber, AccountName: cfg.AccountName, QRPayload: cfg.QRPayload, TopicID: strconv.FormatInt(cfg.TopicID, 10)}
	if f.Mode == "" {
		f.Mode = ModeDisabled
	}
	if cfg.BotID != 0 {
		f.BotID = strconv.FormatInt(cfg.BotID, 10)
	}
	if cfg.ChatID != 0 {
		f.ChatID = strconv.FormatInt(cfg.ChatID, 10)
	}
	if cfg.SenderBotID != 0 {
		f.SenderBotID = strconv.FormatInt(cfg.SenderBotID, 10)
	}
	if !cfg.ActivationAt.IsZero() {
		f.ActivationAt = cfg.ActivationAt.UTC().Format(time.RFC3339)
	}
	return f
}

type AdminSnapshot struct {
	Revision         int64       `json:"revision"`
	Config           AdminFields `json:"config"`
	HasBotToken      bool        `json:"hasBotToken"`
	HasWebhookSecret bool        `json:"hasWebhookSecret"`
	LastMessageAt    *time.Time  `json:"lastMessageAt"`
	ReviewCount      int64       `json:"reviewCount"`
}

func (s *Service) AdminConfig(ctx context.Context) (AdminSnapshot, error) {
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return AdminSnapshot{}, err
	}
	f := fieldsFromConfig(snap.cfg)
	if snap.fields != nil {
		f = *snap.fields
	}
	result := AdminSnapshot{Revision: snap.revision, Config: f, HasBotToken: snap.token != "", HasWebhookSecret: snap.cfg.WebhookSecret != ""}
	if snap.store != nil {
		if f.StoreKey != "" {
			result.LastMessageAt, err = snap.store.LastSePayMessageAt(ctx, f.StoreKey)
			if err != nil {
				return AdminSnapshot{}, err
			}
		}
		result.ReviewCount, err = snap.store.SePayReviewCount(ctx)
	}
	return result, err
}

func validateDraft(f AdminFields) error {
	if f.Mode != ModeDisabled && f.Mode != ModeObserve && f.Mode != ModeActive {
		return &FieldError{"mode"}
	}
	for _, v := range []struct {
		key, value string
		max        int
	}{
		{"storeKey", f.StoreKey, 48}, {"storeName", f.StoreName, 200}, {"bankCode", f.BankCode, 100}, {"bankName", f.BankName, 200}, {"accountNumber", f.AccountNumber, 200}, {"notificationAccountNumber", f.NotificationAccountNumber, 200}, {"accountName", f.AccountName, 200}, {"qrPayload", f.QRPayload, 4096},
	} {
		if !utf8.ValidString(v.value) || len(v.value) > v.max || strings.ContainsAny(v.value, "\r\n\x00") || strings.TrimSpace(v.value) != v.value {
			return &FieldError{v.key}
		}
	}
	if f.StoreKey != "" && !storeKeyPattern.MatchString(f.StoreKey) {
		return &FieldError{"storeKey"}
	}
	if f.AccountNumber != "" && !accountPattern.MatchString(f.AccountNumber) {
		return &FieldError{"accountNumber"}
	}
	if f.NotificationAccountNumber != "" && !accountPattern.MatchString(f.NotificationAccountNumber) {
		return &FieldError{"notificationAccountNumber"}
	}
	if f.QRPayload != "" && (urlSchemePattern.MatchString(f.QRPayload) || strings.HasPrefix(f.QRPayload, "/") || strings.HasPrefix(strings.ToLower(f.QRPayload), "www.")) {
		return &FieldError{"qrPayload"}
	}
	return nil
}

func adminRuntime(f AdminFields, secret string) (Config, error) {
	if err := validateDraft(f); err != nil {
		return Config{}, err
	}
	cfg := Config{Mode: f.Mode, StoreKey: f.StoreKey, StoreName: f.StoreName, BankCode: f.BankCode, BankName: f.BankName, AccountNumber: f.AccountNumber, NotificationAccountNumber: f.NotificationAccountNumber, AccountName: f.AccountName, QRPayload: f.QRPayload, WebhookSecret: secret}
	for _, v := range []struct {
		key, value string
		dest       *int64
		sign       int
	}{
		{"botId", f.BotID, &cfg.BotID, 1}, {"chatId", f.ChatID, &cfg.ChatID, -1}, {"senderBotId", f.SenderBotID, &cfg.SenderBotID, 1}, {"topicId", f.TopicID, &cfg.TopicID, 0},
	} {
		if v.value == "" && f.Mode == ModeDisabled {
			continue
		}
		n, err := strconv.ParseInt(v.value, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != v.value || (v.sign == 1 && n <= 0) || (v.sign == -1 && n >= 0) || (v.sign == 0 && n < 0) {
			return Config{}, &FieldError{v.key}
		}
		*v.dest = n
	}
	if f.ActivationAt != "" {
		parsed, err := time.Parse(time.RFC3339, f.ActivationAt)
		if err != nil || !strings.HasSuffix(f.ActivationAt, "Z") || parsed.IsZero() {
			return Config{}, &FieldError{"activationAt"}
		}
		cfg.ActivationAt = parsed
	}
	if f.Mode == ModeDisabled {
		return cfg, nil
	}
	for _, v := range []struct{ key, value string }{{"storeKey", cfg.StoreKey}, {"storeName", cfg.StoreName}, {"bankCode", cfg.BankCode}, {"bankName", cfg.BankName}, {"accountNumber", cfg.AccountNumber}, {"accountName", cfg.AccountName}, {"qrPayload", cfg.QRPayload}} {
		if v.value == "" {
			return Config{}, &FieldError{v.key}
		}
	}
	if cfg.ActivationAt.IsZero() {
		return Config{}, &FieldError{"activationAt"}
	}
	if f.Mode == ModeActive && !f.ReceiverVerified {
		return Config{}, &FieldError{"receiverVerified"}
	}
	if f.Mode == ModeActive && !f.SourceSeparated {
		return Config{}, &FieldError{"sourceSeparated"}
	}
	// Reuse strict legacy runtime validation instead of creating a second ingest policy.
	raw, err := json.Marshal(cfg)
	if err != nil {
		return Config{}, storage.ErrSePayConfigUnavailable
	}
	_, err = ParseConfig(string(raw))
	if err != nil {
		return Config{}, storage.ErrSePayConfigUnavailable
	}
	return cfg, nil
}

func (s *Service) SaveAdminConfig(ctx context.Context, revision int64, fields AdminFields, token string, actor storage.PaymentProviderActor) (AdminSnapshot, error) {
	if s == nil || s.store == nil {
		return AdminSnapshot{}, ErrUnavailable
	}
	if revision < 0 || revision >= 9007199254740991 {
		return AdminSnapshot{}, &FieldError{"revision"}
	}
	old, err := s.Snapshot(ctx)
	if err != nil {
		return AdminSnapshot{}, err
	}
	if revision != old.revision {
		return AdminSnapshot{}, storage.ErrSePayRevisionConflict
	}
	if token == "" {
		token = old.token
	}
	if len(token) > 512 || strings.ContainsAny(token, "\r\n\x00 /?&#") || (token != "" && !botTokenPattern.MatchString(token)) {
		return AdminSnapshot{}, &FieldError{"botToken"}
	}
	secret := old.cfg.WebhookSecret
	if secret == "" {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return AdminSnapshot{}, storage.ErrSePayConfigUnavailable
		}
		secret = base64.RawURLEncoding.EncodeToString(random)
	}
	if fields.Mode == ModeActive && old.cfg.Mode == ModeActive {
		// Receiver matching changes must not move the live activation boundary.
		fields.ActivationAt = fieldsFromConfig(old.cfg).ActivationAt
	} else if fields.Mode != ModeDisabled && (fields.ActivationAt == "" || (fields.Mode == ModeActive && fields.ActivationAt == fieldsFromConfig(old.cfg).ActivationAt)) {
		fields.ActivationAt = time.Now().UTC().Format(time.RFC3339)
	}
	cfg, err := adminRuntime(fields, secret)
	if err != nil {
		return AdminSnapshot{}, err
	}
	raw, err := json.Marshal(managedFields{Fields: fields, WebhookSecret: secret})
	if err != nil {
		return AdminSnapshot{}, storage.ErrSePayConfigUnavailable
	}
	next, err := s.store.SaveSePayManagedConfig(ctx, storage.SePayManagedConfig{Revision: revision, ConfigJSON: raw, BotToken: token}, cfg.StoreKey, cfg.BankCode, cfg.AccountNumber, actor)
	if err != nil {
		return AdminSnapshot{}, err
	}
	saved := *old
	saved.cfg, saved.revision, saved.fields, saved.token, saved.pinned = cfg, next, &fields, token, true
	return saved.AdminConfig(ctx)
}
