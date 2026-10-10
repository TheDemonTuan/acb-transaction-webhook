package httpapi

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/thedemontuan/acb-transaction-webhook/internal/auth"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func paymentProviderActor(r *http.Request) storage.PaymentProviderActor {
	identity, _ := auth.FromContext(r.Context())
	return storage.PaymentProviderActor{Subject: identity.Subject, Role: string(identity.Role), RequestID: requestIDFromContext(r.Context())}
}

func (s *Server) paymentProviderConfig(w http.ResponseWriter, r *http.Request) {
	if s.payments == nil {
		paymentHTTPError(w, payments.ErrPaymentUnavailable)
		return
	}
	snapshot, err := s.payments.ProviderConfig(r.Context())
	if err != nil {
		paymentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) savePaymentProviderConfig(w http.ResponseWriter, r *http.Request) {
	if s.payments == nil {
		paymentHTTPError(w, payments.ErrPaymentUnavailable)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		paymentBadRequest(w, "INVALID_CONTENT_TYPE")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, 0)
	if err != nil {
		paymentBadRequest(w, "INVALID_PROVIDER_CONFIG")
		return
	}
	if _, err = decoder.Token(); err != io.EOF {
		paymentBadRequest(w, "INVALID_PROVIDER_CONFIG")
		return
	}
	fields, ok := value.(map[string]any)
	if !ok || len(fields) != 4 {
		paymentBadRequest(w, "INVALID_PROVIDER_CONFIG")
		return
	}
	clientID, clientOK := fields["clientId"].(string)
	apiKey, apiOK := fields["apiKey"].(string)
	checksumKey, checksumOK := fields["checksumKey"].(string)
	enabled, enabledOK := fields["enabled"].(bool)
	if !clientOK || !apiOK || !checksumOK || !enabledOK {
		paymentBadRequest(w, "INVALID_PROVIDER_CONFIG")
		return
	}
	snapshot, err := s.payments.SaveProviderConfig(r.Context(), storage.PaymentProviderCredentials{ClientID: clientID, APIKey: apiKey, ChecksumKey: checksumKey}, enabled, paymentProviderActor(r))
	if err != nil {
		paymentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
