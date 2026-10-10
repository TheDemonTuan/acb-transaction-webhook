package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// PaymentCommitNotifier runs only after the financial transaction commits.
// Publish or dispatcher wake failures never turn an accepted receipt into an
// HTTP failure: journal replay and durable dispatcher polling recover them.
func (s *Server) PaymentCommitNotifier() func(storage.EventNotification) {
	return func(event storage.EventNotification) {
		s.Publish(eventhub.Event{Seq: event.JournalSeq, Epoch: event.Epoch, EventType: event.EventType, AggregateID: event.TransactionID, Payload: event.Payload, CreatedAt: event.CreatedAt, CommittedAt: event.CommittedAt})
		if s.wakeFn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.wakeFn(ctx)
		}
	}
}

func paymentResponseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/payments") || strings.HasPrefix(r.URL.Path, "/api/public/v1/payments") || strings.HasPrefix(r.URL.Path, "/api/v1/payment-provider/") || r.URL.Path == "/api/v1/payment-reviews" || r.URL.Path == "/api/public/v1/payment-config" || r.URL.Path == "/api/v1/status" {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
		}
		next.ServeHTTP(w, r)
	})
}

// Production ingress MUST be tunnel-only and strip client-supplied CF headers.
// Only that proxy boundary makes CF-Connecting-IP authoritative; XFF is never used.
func (s *Server) paymentClientIP(r *http.Request) string {
	if s.cfg.Production {
		if ip := net.ParseIP(r.Header.Get("CF-Connecting-IP")); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	if ip := net.ParseIP(r.RemoteAddr); ip != nil {
		return ip.String()
	}
	return "unknown"
}

func paymentHTTPError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE"
	var safe *payments.ServiceError
	if errors.As(err, &safe) {
		status, code = safe.HTTPStatus, safe.Code
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, status, map[string]string{"error": code, "code": code})
}

func paymentBadRequest(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "code": code})
}

func (s *Server) publicPaymentConfig(w http.ResponseWriter, r *http.Request) {
	service := s.payments
	if service == nil {
		service = payments.NewManagedService(s.cfg, s.store, nil)
	}
	config := service.Config()
	status, err := service.Status(r.Context())
	if err != nil || status.Status == "UNAVAILABLE" {
		config.Ready, config.Status = false, "UNAVAILABLE"
	}
	writeJSON(w, http.StatusOK, config)
}

func decodePaymentRequest(w http.ResponseWriter, r *http.Request, public bool) (int64, string, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		paymentBadRequest(w, "INVALID_CONTENT_TYPE")
		return 0, "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, 0)
	if err == nil {
		_, err = decoder.Token()
		if err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("trailing JSON")
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "BODY_TOO_LARGE", "code": "BODY_TOO_LARGE"})
		return 0, "", false
	}
	body, ok := value.(map[string]any)
	if err != nil || !ok {
		paymentBadRequest(w, "INVALID_REQUEST")
		return 0, "", false
	}
	for field := range body {
		if field != "amountVnd" && !(public && field == "origin") {
			paymentBadRequest(w, "INVALID_REQUEST")
			return 0, "", false
		}
	}
	number, ok := body["amountVnd"].(json.Number)
	amount, err := number.Int64()
	if !ok || err != nil {
		paymentBadRequest(w, "INVALID_AMOUNT")
		return 0, "", false
	}
	origin := "OPERATOR_DYNAMIC"
	if public {
		origin, ok = body["origin"].(string)
		if !ok {
			paymentBadRequest(w, "INVALID_ORIGIN")
			return 0, "", false
		}
	}
	return amount, origin, true
}

func (s *Server) createPublicPayment(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PaymentPublicOrigin == "" || r.Header.Get("Origin") != s.cfg.PaymentPublicOrigin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ORIGIN_MISMATCH", "code": "ORIGIN_MISMATCH"})
		return
	}
	s.createPayment(w, r, true)
}

func (s *Server) createAdminPayment(w http.ResponseWriter, r *http.Request) {
	s.createPayment(w, r, false)
}

func (s *Server) createPayment(w http.ResponseWriter, r *http.Request, public bool) {
	amount, origin, ok := decodePaymentRequest(w, r, public)
	if !ok {
		return
	}
	if s.payments == nil {
		paymentHTTPError(w, payments.ErrPaymentUnavailable)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	// Bounded lock stripes serialize identical HTTP intents without an unbounded
	// key map. Durable storage still arbitrates across gateway instances.
	hash := sha256.Sum256([]byte(key))
	lock := &s.paymentIntentLocks[hash[0]%byte(len(s.paymentIntentLocks))]
	lock.Lock()
	defer lock.Unlock()
	order, replay, err := s.payments.ExistingOrderIntent(r.Context(), amount, origin, key)
	if err != nil {
		paymentHTTPError(w, err)
		return
	}
	if replay {
		writeJSON(w, http.StatusOK, order)
		return
	}
	if public && !s.paymentCreateLimiter.allow(s.paymentClientIP(r), 6, time.Minute, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "RATE_LIMITED", "code": "RATE_LIMITED"})
		return
	}
	order, created, err := s.payments.CreateOrder(r.Context(), amount, origin, key)
	if err != nil {
		paymentHTTPError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if order.Status == "CREATING" {
			status = http.StatusAccepted
		}
	}
	writeJSON(w, status, order)
}

func validPaymentCapability(id string) bool {
	if len(id) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == id
}

func (s *Server) publicPayment(w http.ResponseWriter, r *http.Request) {
	if !s.paymentGetLimiter.allow(s.paymentClientIP(r), 60, time.Minute, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "RATE_LIMITED", "code": "RATE_LIMITED"})
		return
	}
	s.payment(w, r)
}

func (s *Server) payment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validPaymentCapability(id) {
		paymentHTTPError(w, payments.ErrPaymentNotFound)
		return
	}
	service := s.payments
	if service == nil {
		service = payments.NewManagedService(s.cfg, s.store, nil)
	}
	order, err := service.Order(r.Context(), id)
	if err != nil {
		paymentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (s *Server) paymentOrders(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		paymentBadRequest(w, "INVALID_PAGINATION")
		return
	}
	service := s.payments
	if service == nil {
		service = payments.NewManagedService(s.cfg, s.store, nil)
	}
	page, err := service.Orders(r.Context(), r.URL.Query().Get("status"), cursor, limit)
	if err != nil {
		if errors.Is(err, storage.ErrInvalidPaymentOrder) || cursor != "" {
			paymentBadRequest(w, "INVALID_PAGINATION")
		} else {
			paymentHTTPError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) cancelPayment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !validPaymentCapability(id) {
		paymentHTTPError(w, payments.ErrPaymentNotFound)
		return
	}
	if s.payments == nil {
		paymentHTTPError(w, payments.ErrPaymentUnavailable)
		return
	}
	order, err := s.payments.CancelOrder(r.Context(), id)
	if err != nil {
		paymentHTTPError(w, err)
		return
	}
	status := http.StatusOK
	if order.Status != "CANCELLED" && order.Status != "EXPIRED" && order.Status != "FAILED" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, order)
}

func (s *Server) paymentReviews(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		paymentBadRequest(w, "INVALID_PAGINATION")
		return
	}
	page, err := s.store.ListPaymentReviews(r.Context(), cursor, limit)
	if err != nil {
		if cursor != "" {
			paymentBadRequest(w, "INVALID_PAGINATION")
		} else {
			paymentHTTPError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) confirmPaymentWebhook(w http.ResponseWriter, r *http.Request) {
	// Empty body or {} only: no URL override or other browser configuration.
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
		if err != nil {
			paymentBadRequest(w, "INVALID_REQUEST")
			return
		}
		if strings.TrimSpace(string(body)) != "" && strings.TrimSpace(string(body)) != "{}" {
			paymentBadRequest(w, "INVALID_REQUEST")
			return
		}
	}
	if s.payments == nil {
		paymentHTTPError(w, payments.ErrPaymentUnavailable)
		return
	}
	if err := s.payments.ConfirmProviderWebhook(r.Context(), paymentProviderActor(r)); err != nil {
		paymentHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"confirmed": true})
}

func (s *Server) requirePaymentMutationAllowed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil {
			paymentHTTPError(w, payments.ErrPaymentUnavailable)
			return
		}
		if err := s.store.CheckMutationAllowed(r.Context()); err != nil {
			paymentHTTPError(w, payments.ErrPaymentUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
