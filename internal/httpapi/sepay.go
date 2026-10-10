package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// SePayStoreConfigResponse exposes only the receiver approved for public use.
// ACTIVE describes ingest configuration, not bank connectivity or delivery SLA.
type SePayStoreConfigResponse struct {
	Provider      string     `json:"provider"`
	Status        string     `json:"status"`
	StoreName     string     `json:"storeName"`
	Bank          string     `json:"bank"`
	AccountNumber string     `json:"accountNumber"`
	AccountName   string     `json:"accountName"`
	QRPayload     string     `json:"qrPayload"`
	LastMessageAt *time.Time `json:"lastMessageAt"`
}

// WithSePay attaches the independent Store notification service before serving.
func (s *Server) WithSePay(service *sepay.Service) *Server {
	s.sepay = service
	return s
}

func (s *Server) publicSePayStore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	response := SePayStoreConfigResponse{Provider: "SEPAY", Status: "DISABLED"}
	var cfg sepay.Config
	if s.sepay != nil {
		cfg = s.sepay.Config()
	}
	switch cfg.Mode {
	case sepay.ModeObserve:
		response.Status = "OBSERVING"
	case sepay.ModeActive:
		response.Status = "ACTIVE"
		response.StoreName = cfg.StoreName
		response.Bank = cfg.BankCode
		response.AccountNumber = cfg.AccountNumber
		response.AccountName = cfg.AccountName
		response.QRPayload = cfg.QRPayload
	}
	if cfg.Mode != "" && cfg.Mode != sepay.ModeDisabled && s.store != nil {
		lastMessageAt, err := s.store.LastSePayMessageAt(r.Context(), cfg.StoreKey)
		if err != nil {
			writeSePayWebhookError(w, http.StatusServiceUnavailable, "SEPAY_UNAVAILABLE")
			return
		}
		response.LastMessageAt = lastMessageAt
	}
	writeJSON(w, http.StatusOK, response)
}

const maxSePayWebhookBytes = 64 << 10

// The callback authenticates its Telegram secret before reading untrusted JSON.
// HandleUpdate returns only after ignored input or a durable storage commit.
func (s *Server) sepayTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.sepay == nil || !s.sepay.Enabled() {
		writeSePayWebhookError(w, http.StatusServiceUnavailable, "SEPAY_UNAVAILABLE")
		return
	}
	if !s.sepay.Authenticate(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")) {
		writeSePayWebhookError(w, http.StatusUnauthorized, "INVALID_SECRET")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSePayWebhookBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeSePayWebhookError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
		} else {
			writeSePayWebhookError(w, http.StatusBadRequest, "INVALID_WEBHOOK")
		}
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, 0)
	if err == nil {
		if _, trailing := decoder.Token(); trailing != io.EOF {
			err = errors.New("trailing JSON")
		}
	}
	if _, object := value.(map[string]any); err != nil || !object {
		writeSePayWebhookError(w, http.StatusBadRequest, "INVALID_WEBHOOK")
		return
	}
	update, err := sepay.DecodeTelegramUpdate(raw)
	if err != nil {
		writeSePayWebhookError(w, http.StatusBadRequest, "INVALID_WEBHOOK")
		return
	}
	if err := s.sepay.HandleUpdate(r.Context(), update); err != nil {
		writeSePayWebhookError(w, http.StatusServiceUnavailable, "SEPAY_UNAVAILABLE")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeSePayWebhookError(w http.ResponseWriter, status int, code string) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, status, map[string]string{"error": code})
}

type SePayStatusResponse struct {
	Mode          string     `json:"mode"`
	LastMessageAt *time.Time `json:"lastMessageAt"`
	ReviewCount   int64      `json:"reviewCount"`
}

func (s *Server) sepayStatus(r *http.Request) (SePayStatusResponse, error) {
	response := SePayStatusResponse{Mode: sepay.ModeDisabled}
	if s.sepay != nil {
		cfg := s.sepay.Config()
		if cfg.Mode != "" {
			response.Mode = cfg.Mode
		}
		if cfg.Mode == sepay.ModeObserve || cfg.Mode == sepay.ModeActive {
			last, err := s.store.LastSePayMessageAt(r.Context(), cfg.StoreKey)
			if err != nil {
				return response, err
			}
			response.LastMessageAt = last
		}
	}
	count, err := s.store.SePayReviewCount(r.Context())
	response.ReviewCount = count
	return response, err
}

func (s *Server) sepayReviews(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PAGINATION")
		return
	}
	page, err := s.store.ListSePayReviews(r.Context(), cursor, limit)
	if errors.Is(err, storage.ErrInvalidSePayReviewCursor) {
		writeError(w, http.StatusBadRequest, "INVALID_PAGINATION")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
