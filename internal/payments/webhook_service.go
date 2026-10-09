package payments

import (
	"context"
	"database/sql"
	"errors"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

var ErrWebhookUnavailable = errors.New("payment webhook temporarily unavailable")

// WithReconcileWake installs the gateway-to-worker wake hook. A wake is merely a
// hint: durable inbox/order schedules are authoritative if RPC is unavailable.
func (s *Service) WithReconcileWake(wake func(context.Context) error) *Service {
	s.reconcileWake = wake
	return s
}

func (s *Service) wakeReconciler(ctx context.Context) {
	s.Wake()
	if s.reconcileWake != nil {
		_ = s.reconcileWake(ctx)
	}
}

// HandleWebhook verifies the complete data with the SDK before recognizing even
// the confirmation sample. Every ACK represents either a financial commit, a
// durable review record, or the exact verified nonfinancial confirmation sample.
func (s *Service) HandleWebhook(ctx context.Context, body map[string]any) (resultErr error) {
	if s.managed {
		operation, operationCtx, done, err := s.pin(ctx)
		if err != nil {
			return ErrWebhookUnavailable
		}
		defer done()
		return operation.HandleWebhook(operationCtx, body)
	}
	if !validWebhookShape(body) {
		return ErrInvalidWebhook
	}
	if s.provider == nil || s.store == nil || s.cfg.PayOSClientID == "" {
		return ErrWebhookUnavailable
	}
	data, err := s.verifyWebhook(ctx, body)
	if err != nil {
		if errors.Is(err, ErrInvalidSignature) || errors.Is(err, ErrInvalidWebhook) {
			return err
		}
		return ErrWebhookUnavailable
	}
	if data == nil {
		return ErrInvalidWebhook
	}
	// Count verified callbacks through their commit and post-commit hints so
	// quiesce cannot report drained while an inbox/financial write is active.
	if err := s.beginPaymentRequest(ctx); err != nil {
		return ErrWebhookUnavailable
	}
	defer s.endPaymentRequest()
	defer func() {
		if resultErr == nil {
			s.recordActivity(ctx, "WEBHOOK")
		}
	}()
	raw := body["data"].(map[string]any)
	callback := storage.VerifiedPaymentCallback{Data: raw, OrderCode: data.OrderCode, PaymentLinkID: data.PaymentLinkId, Reference: data.Reference}
	if body["code"] != "00" || body["success"] != true || data.Code != "00" {
		return s.saveWebhookReview(ctx, callback, "NON_SUCCESS")
	}
	if isPayOSConfirmationSample(data) {
		if err := s.store.CheckMutationAllowed(ctx); err != nil {
			return ErrWebhookUnavailable
		}
		return nil
	}
	transactionAt, err := parsePaymentTransactionTime(data.TransactionDateTime)
	if err != nil {
		return s.saveWebhookReview(ctx, callback, "INVALID_TRANSACTION_DATE")
	}
	if data.Currency != "VND" || data.Reference == "" || data.PaymentLinkId == "" || data.Amount <= 0 || data.OrderCode <= 0 {
		return s.saveWebhookReview(ctx, callback, "PAYMENT_MISMATCH")
	}
	virtualAccount := ""
	if data.VirtualAccountNumber != nil {
		virtualAccount = *data.VirtualAccountNumber
	}
	in := storage.SettlementInput{
		ChannelID: s.cfg.PayOSClientID, ChannelNamespace: s.channelNamespace,
		OrderCode: data.OrderCode, PaymentLinkID: data.PaymentLinkId, Reference: data.Reference,
		AmountVnd: int64(data.Amount), TransactionAt: transactionAt, Description: data.Description,
		AccountNumber: data.AccountNumber, VirtualAccountNumber: virtualAccount,
		Source: "REALTIME", VerifiedCallback: &callback,
	}
	order, err := s.store.PaymentOrderByCode(ctx, data.OrderCode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ErrWebhookUnavailable
	}
	var result storage.SettlementResult
	if err == nil && (order.Status == "CANCELLED" || order.Status == "EXPIRED") {
		result, err = s.confirmWebhookPayment(ctx, order, data, in)
	} else {
		result, err = s.store.SettlePayment(ctx, in)
	}
	if err != nil {
		return ErrWebhookUnavailable
	}
	// Publishing cannot change the committed financial outcome. The supplied
	// callback is a post-commit hint; durable journal/outbox consumers recover.
	if result.Event != nil && s.onCommit != nil {
		s.onCommit(*result.Event)
	}
	if result.ReviewReason == "AWAITING_ORDER_BIND" || result.ReviewReason == "PAYMENT_DETAILS_PENDING" {
		s.wakeReconciler(ctx)
	}
	return nil
}

func (s *Service) saveWebhookReview(ctx context.Context, in storage.VerifiedPaymentCallback, reason string) error {
	in.Reason = reason
	if _, err := s.store.SaveVerifiedPaymentCallback(ctx, in); err != nil {
		return ErrWebhookUnavailable
	}
	if reason == "AWAITING_ORDER_BIND" || reason == "PAYMENT_DETAILS_PENDING" {
		s.wakeReconciler(ctx)
	}
	return nil
}

func validWebhookShape(body map[string]any) bool {
	if body == nil || !validJSONNumbers(body) {
		return false
	}
	if _, ok := body["code"].(string); !ok {
		return false
	}
	if _, ok := body["success"].(bool); !ok {
		return false
	}
	if value, present := body["desc"]; present {
		if _, ok := value.(string); !ok {
			return false
		}
	}
	data, ok := body["data"].(map[string]any)
	if !ok || data == nil || !safeNumber(data["orderCode"]) || !safeNumber(data["amount"]) {
		return false
	}
	for _, key := range []string{"description", "accountNumber", "reference", "transactionDateTime", "currency", "paymentLinkId", "code", "desc"} {
		if value, present := data[key]; present {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	for _, key := range []string{"counterAccountBankId", "counterAccountBankName", "counterAccountName", "counterAccountNumber", "virtualAccountName", "virtualAccountNumber"} {
		if value, present := data[key]; present && value != nil {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	return true
}

func isPayOSConfirmationSample(data *payos.WebhookData) bool {
	return data.OrderCode == 123 && data.Amount == 3000 && data.Description == "VQRIO123" &&
		data.AccountNumber == "12345678" && data.Reference == "TF230204212323" &&
		data.TransactionDateTime == "2023-02-04 18:25:00" && data.Currency == "VND" &&
		data.PaymentLinkId == "124c33293c43417ab7879e14c8d9eb18" && data.Code == "00"
}
