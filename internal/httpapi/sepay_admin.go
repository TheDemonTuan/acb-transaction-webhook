package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func sepayAdminError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "SEPAY_UNAVAILABLE"
	var field *sepay.FieldError
	switch {
	case errors.As(err, &field):
		status, code = http.StatusBadRequest, "INVALID_SEPAY_CONFIG"
	case errors.Is(err, storage.ErrSePayRevisionConflict):
		status, code = http.StatusConflict, "SEPAY_REVISION_CONFLICT"
	case errors.Is(err, storage.ErrSePayReceiverMismatch):
		status, code = http.StatusConflict, "SEPAY_RECEIVER_IMMUTABLE"
	case errors.Is(err, sepay.ErrTelegramTokenRequired):
		status, code = http.StatusBadRequest, "TELEGRAM_TOKEN_REQUIRED"
	case errors.Is(err, sepay.ErrTelegramConfigRequired):
		status, code = http.StatusBadRequest, "TELEGRAM_CONFIG_REQUIRED"
	case errors.Is(err, sepay.ErrTelegramBotMismatch):
		status, code = http.StatusBadRequest, "TELEGRAM_BOT_MISMATCH"
	case errors.Is(err, sepay.ErrTelegramForeignWebhook):
		status, code = http.StatusConflict, "TELEGRAM_FOREIGN_WEBHOOK"
	case errors.Is(err, sepay.ErrTelegramRequest):
		status, code = http.StatusBadGateway, "TELEGRAM_REQUEST_FAILED"
	}
	response := map[string]string{"error": code}
	if field != nil {
		response["field"] = field.Field
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, status, response)
}

func (s *Server) sepayAdminConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := s.sepay.AdminConfig(r.Context())
	if err != nil {
		sepayAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) saveSePayAdminConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, http.StatusBadRequest, "INVALID_CONTENT_TYPE")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
		} else {
			writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
		}
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
		return
	}
	if _, err := decoder.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
		return
	}
	fields, ok := value.(map[string]any)
	if !ok || len(fields) != 3 || fields["revision"] == nil || fields["config"] == nil || fields["botToken"] == nil {
		writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
		return
	}
	configFields, ok := fields["config"].(map[string]any)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
		return
	}
	for key, val := range configFields {
		if val == nil {
			writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
			return
		}
		switch key {
		case "mode", "storeKey", "storeName", "bankCode", "bankName", "accountNumber", "notificationAccountNumber", "accountName", "qrPayload", "botId", "chatId", "senderBotId", "topicId", "activationAt":
			if _, ok := val.(string); !ok {
				writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
				return
			}
		case "receiverVerified", "sourceSeparated":
			if _, ok := val.(bool); !ok {
				writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
			return
		}
	}
	if _, ok := configFields["mode"]; !ok {
		writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
		return
	}
	var body struct {
		Revision int64             `json:"revision"`
		Config   sepay.AdminFields `json:"config"`
		BotToken string            `json:"botToken"`
	}
	existing, err := s.sepay.AdminConfig(r.Context())
	if err != nil {
		sepayAdminError(w, err)
		return
	}
	body.Config = existing.Config
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if strict.Decode(&body) != nil {
		writeError(w, http.StatusBadRequest, "INVALID_SEPAY_CONFIG")
		return
	}
	oldNotificationAccount := existing.Config.NotificationAccountNumber
	if oldNotificationAccount == "" {
		oldNotificationAccount = existing.Config.AccountNumber
	}
	nextNotificationAccount := body.Config.NotificationAccountNumber
	if nextNotificationAccount == "" {
		nextNotificationAccount = body.Config.AccountNumber
	}
	if body.Config.Mode == sepay.ModeActive && nextNotificationAccount != oldNotificationAccount && configFields["sourceSeparated"] != true {
		// A partial update must not silently inherit an old source-isolation attestation.
		sepayAdminError(w, &sepay.FieldError{Field: "sourceSeparated"})
		return
	}
	result, err := s.sepay.SaveAdminConfig(r.Context(), body.Revision, body.Config, body.BotToken, paymentProviderActor(r))
	if err != nil {
		sepayAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) registerSePayTelegram(w http.ResponseWriter, r *http.Request) {
	s.sepayTelegramOperation(w, r, true)
}

func (s *Server) sepayTelegramStatus(w http.ResponseWriter, r *http.Request) {
	s.sepayTelegramOperation(w, r, false)
}

func (s *Server) sepayTelegramOperation(w http.ResponseWriter, r *http.Request, register bool) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := s.sepay.Telegram(r.Context(), s.cfg.PaymentPublicOrigin, register)
	if err != nil {
		sepayAdminError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
