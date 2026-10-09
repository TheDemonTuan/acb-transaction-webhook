package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
)

const maxPayOSWebhookBytes = 64 << 10

func (s *Server) WithPayments(service *payments.Service) *Server {
	s.payments = service
	return s
}

// The callback is deliberately outside owner authentication and CSRF. Authenticity
// is provided by the SDK signature verification, never by browser credentials.
func (s *Server) payOSWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxPayOSWebhookBytes)
	encoded, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writePayOSWebhookError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
		} else {
			writePayOSWebhookError(w, http.StatusBadRequest, "INVALID_WEBHOOK")
		}
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	value, err := decodePayOSJSONValue(decoder, 0)
	if err == nil {
		_, trailing := decoder.Token()
		if trailing != io.EOF {
			if trailing == nil {
				trailing = errors.New("trailing JSON")
			}
			err = trailing
		}
	}
	body, object := value.(map[string]any)
	if err != nil || !object {
		writePayOSWebhookError(w, http.StatusBadRequest, "INVALID_WEBHOOK")
		return
	}
	if s.payments == nil {
		writePayOSWebhookError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE")
		return
	}
	err = s.payments.HandleWebhook(r.Context(), body)
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case errors.Is(err, payments.ErrInvalidWebhook):
		writePayOSWebhookError(w, http.StatusBadRequest, "INVALID_WEBHOOK")
	case errors.Is(err, payments.ErrInvalidSignature):
		writePayOSWebhookError(w, http.StatusUnauthorized, "INVALID_SIGNATURE")
	default:
		writePayOSWebhookError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE")
	}
}

func writePayOSWebhookError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// Token decoding detects duplicate keys at every depth before a map can silently
// overwrite them. The depth bound also prevents untrusted JSON recursion overflow.
func decodePayOSJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("JSON nesting too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate object key")
			}
			value, err := decodePayOSJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			if err == nil {
				err = errors.New("invalid object closing")
			}
			return nil, err
		}
		return object, nil
	case '[':
		array := []any{}
		for decoder.More() {
			value, err := decodePayOSJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			if err == nil {
				err = errors.New("invalid array closing")
			}
			return nil, err
		}
		return array, nil
	default:
		return nil, errors.New("invalid JSON delimiter")
	}
}
