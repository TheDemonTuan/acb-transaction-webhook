package payments

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// ServiceError is safe to expose at the HTTP boundary. It never wraps provider
// bodies, database errors, credentials or the capability URL.
type ServiceError struct {
	Code       string
	HTTPStatus int
}

func (e *ServiceError) Error() string { return e.Code }

var (
	ErrInvalidAmount         = &ServiceError{Code: "INVALID_AMOUNT", HTTPStatus: http.StatusBadRequest}
	ErrInvalidOrigin         = &ServiceError{Code: "INVALID_ORIGIN", HTTPStatus: http.StatusBadRequest}
	ErrInvalidIdempotencyKey = &ServiceError{Code: "INVALID_IDEMPOTENCY_KEY", HTTPStatus: http.StatusBadRequest}
	ErrIdempotencyConflict   = &ServiceError{Code: "IDEMPOTENCY_CONFLICT", HTTPStatus: http.StatusConflict}
	ErrPaymentUnavailable    = &ServiceError{Code: "PAYMENT_UNAVAILABLE", HTTPStatus: http.StatusServiceUnavailable}
	ErrPaymentsDisabled      = &ServiceError{Code: "PAYMENTS_DISABLED", HTTPStatus: http.StatusServiceUnavailable}
	ErrWebhookUnconfirmed    = &ServiceError{Code: "WEBHOOK_UNCONFIRMED", HTTPStatus: http.StatusServiceUnavailable}
)

// CreateOrder persists and leases one payment intent before any provider I/O.
// Replays never call Create, even after the lease expires: step-three recovery
// must first obtain provider evidence with Get using the same stored order code.
// An uncertain outcome is a successful persisted CREATING snapshot, not a failed
// payment; HTTP callers return 202 for new CREATING orders and 200 for replays.
func (s *Service) CreateOrder(ctx context.Context, amountVnd int64, origin, idempotencyKey string) (storage.PaymentOrder, bool, error) {
	if amountVnd < 1 || amountVnd > s.cfg.PaymentMaxAmountVND || amountVnd > maxSafeInteger || int64(int(amountVnd)) != amountVnd {
		return storage.PaymentOrder{}, false, ErrInvalidAmount
	}
	if origin != "STATIC_URL" && origin != "OPERATOR_DYNAMIC" {
		return storage.PaymentOrder{}, false, ErrInvalidOrigin
	}
	if !canonicalUUID(idempotencyKey) {
		return storage.PaymentOrder{}, false, ErrInvalidIdempotencyKey
	}
	if s.store == nil {
		return storage.PaymentOrder{}, false, ErrPaymentUnavailable
	}
	hash := paymentRequestHash(amountVnd, origin)
	// Operational creation gates must not make an issued capability inaccessible
	// or turn an idempotent retry into a new provider operation.
	existing, err := s.store.PaymentOrderByKey(ctx, s.cfg.PayOSClientID, idempotencyKey)
	if err == nil {
		if existing.RequestHash != hash {
			return storage.PaymentOrder{}, false, ErrIdempotencyConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return storage.PaymentOrder{}, false, ErrPaymentUnavailable
	}
	switch s.Config().Status {
	case "DISABLED":
		return storage.PaymentOrder{}, false, ErrPaymentsDisabled
	case "WEBHOOK_UNCONFIRMED":
		return storage.PaymentOrder{}, false, ErrWebhookUnconfirmed
	case "READY":
	default:
		return storage.PaymentOrder{}, false, ErrPaymentUnavailable
	}
	if err := s.beginPaymentRequest(ctx); err != nil {
		return storage.PaymentOrder{}, false, ErrPaymentUnavailable
	}
	defer s.endPaymentRequest()

	order, created, err := s.store.ReservePaymentOrder(ctx, storage.PaymentOrderIntent{
		ChannelID: s.cfg.PayOSClientID, IdempotencyKey: idempotencyKey,
		RequestHash: hash, AmountVnd: amountVnd, Origin: origin,
	})
	if errors.Is(err, storage.ErrPaymentIdempotencyConflict) {
		return storage.PaymentOrder{}, false, ErrIdempotencyConflict
	}
	if err != nil {
		return storage.PaymentOrder{}, false, ErrPaymentUnavailable
	}
	if !created {
		return order, false, nil
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, order.ExpiresAt)
	if err != nil || int64(int(expiresAt.Unix())) != expiresAt.Unix() {
		return order, true, ErrPaymentUnavailable
	}
	expiresUnix := int(expiresAt.Unix())
	capabilityURL := s.cfg.PaymentPublicOrigin + "/pay/" + order.ID
	requestCtx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	response, providerErr := s.provider.Create(requestCtx, payos.CreatePaymentLinkRequest{
		OrderCode: order.OrderCode, Amount: int(order.AmountVnd), Description: order.Description,
		ReturnUrl: capabilityURL, CancelUrl: capabilityURL, ExpiredAt: &expiresUnix,
	})
	cancel()

	update := storage.PaymentOrderUpdate{Status: "CREATING"}
	leaseUntil, err := time.Parse(time.RFC3339Nano, order.OperationLeaseUntil)
	if err != nil {
		return order, true, ErrPaymentUnavailable
	}
	// Releasing the operation token after uncertainty must not wake a competing
	// Get while an interrupted Create could still be running at the provider.
	update.NextReconcileAt = leaseUntil
	if providerErr != nil {
		update.LastErrorCode = "CREATE_OUTCOME_UNKNOWN"
		var failure *ProviderError
		if errors.As(providerErr, &failure) {
			if !failure.Indeterminate && failure.HTTPStatus >= 400 && failure.HTTPStatus < 500 && failure.HTTPStatus != 408 && failure.HTTPStatus != 429 {
				// A rejection alone is not evidence of absence. Recovery must Get
				// HTTP 404 before marking FAILED, never guess from envelope codes.
				update.LastErrorCode = "CREATE_REJECTED_HTTP_" + strconv.Itoa(failure.HTTPStatus)
			}
			if retryAt := time.Now().UTC().Add(failure.RetryAfter); retryAt.After(update.NextReconcileAt) {
				update.NextReconcileAt = retryAt
			}
		}
	} else if !validCreateResponse(order, response) {
		update.LastErrorCode = "INVALID_CREATE_RESPONSE"
	} else {
		update.Status = "PENDING"
		update.PaymentLinkID = response.PaymentLinkId
		update.QRCode = response.QrCode
		update.CheckoutURL = response.CheckoutUrl
		update.BankBin = response.Bin
		update.AccountNumber = response.AccountNumber
		update.AccountName = response.AccountName
		update.NextReconcileAt = time.Now().UTC().Add(time.Minute)
	}

	// A client disconnect may cancel the request after the provider has created
	// the link. Persist its result with a bounded independent context; the intent
	// and lease already survive even if this write fails or the process crashes.
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer persistCancel()
	persisted, err := s.store.CompletePaymentOrderOperation(persistCtx, order.ID, order.OperationToken, update)
	if errors.Is(err, storage.ErrPaymentOperationLost) {
		persisted, err = s.store.PaymentOrder(persistCtx, order.ID)
	}
	if err != nil {
		return order, true, ErrPaymentUnavailable
	}
	return persisted, true, nil
}

func paymentRequestHash(amountVnd int64, origin string) string {
	// The two allowlisted fields have an unambiguous representation independent
	// of JSON formatting, endpoint identity, request headers and callback URLs.
	sum := sha256.Sum256([]byte(strconv.FormatInt(amountVnd, 10) + "\n" + origin))
	return hex.EncodeToString(sum[:])
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value[14] < '1' || value[14] > '8' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for i := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f')) {
			return false
		}
	}
	return true
}

func validCreateResponse(order storage.PaymentOrder, response *payos.CreatePaymentLinkResponse) bool {
	if response == nil || response.OrderCode != order.OrderCode || int64(response.Amount) != order.AmountVnd || response.Currency != "VND" || response.Status != "PENDING" {
		return false
	}
	if strings.TrimSpace(response.PaymentLinkId) == "" || strings.TrimSpace(response.QrCode) == "" || strings.TrimSpace(response.Bin) == "" || strings.TrimSpace(response.AccountNumber) == "" || strings.TrimSpace(response.AccountName) == "" {
		return false
	}
	checkout, err := url.Parse(response.CheckoutUrl)
	return err == nil && checkout.Scheme == "https" && checkout.Hostname() == "pay.payos.vn" && checkout.User == nil && (checkout.Port() == "" || checkout.Port() == "443")
}
