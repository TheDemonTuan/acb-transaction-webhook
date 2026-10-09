package payments

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type reconcileState struct {
	mu          sync.Mutex
	wake        chan struct{}
	changed     chan struct{}
	pause       chan struct{}
	running     bool
	quiesced    bool
	active      int
	nextRequest time.Time
}

func (s *Service) initReconcileLocked() {
	if s.reconcile.wake == nil {
		s.reconcile.wake = make(chan struct{}, 1)
	}
	if s.reconcile.changed == nil {
		s.reconcile.changed = make(chan struct{})
	}
	if s.reconcile.pause == nil {
		s.reconcile.pause = make(chan struct{})
	}
}

// Start launches at most one local scheduler. Production runs this only in the
// singleton worker; per-order storage leases fence other service instances.
func (s *Service) Start(ctx context.Context) {
	s.reconcile.mu.Lock()
	s.initReconcileLocked()
	if s.reconcile.running {
		s.reconcile.mu.Unlock()
		return
	}
	s.reconcile.running = true
	s.reconcile.mu.Unlock()
	go s.runReconciler(ctx)
}

// Run is the blocking form of Start, for an owner-managed goroutine.
func (s *Service) Run(ctx context.Context) {
	s.reconcile.mu.Lock()
	s.initReconcileLocked()
	if s.reconcile.running {
		s.reconcile.mu.Unlock()
		return
	}
	s.reconcile.running = true
	s.reconcile.mu.Unlock()
	s.runReconciler(ctx)
}

func (s *Service) runReconciler(ctx context.Context) {
	defer func() { s.reconcile.mu.Lock(); s.reconcile.running = false; s.reconcile.mu.Unlock() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = s.reconcileDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.reconcile.wake:
		}
	}
}

func (s *Service) Wake() {
	s.reconcile.mu.Lock()
	s.initReconcileLocked()
	select {
	case s.reconcile.wake <- struct{}{}:
	default:
	}
	s.reconcile.mu.Unlock()
}

// Quiesce denies new operations and drains complete network-and-commit work.
// It does not cancel requests already admitted or abandon their durable results.
func (s *Service) Quiesce(ctx context.Context) error {
	s.reconcile.mu.Lock()
	s.initReconcileLocked()
	if !s.reconcile.quiesced {
		close(s.reconcile.pause)
		s.reconcile.quiesced = true
	}
	for s.reconcile.active != 0 {
		changed := s.reconcile.changed
		s.reconcile.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		s.reconcile.mu.Lock()
	}
	s.reconcile.mu.Unlock()
	return nil
}

func (s *Service) Resume() {
	s.reconcile.mu.Lock()
	if s.reconcile.quiesced {
		s.reconcile.pause = make(chan struct{})
		s.reconcile.quiesced = false
	}
	s.reconcile.mu.Unlock()
	s.Wake()
}

func (s *Service) ActiveRequests() int {
	s.reconcile.mu.Lock()
	defer s.reconcile.mu.Unlock()
	return s.reconcile.active
}

func (s *Service) beginPaymentRequest(ctx context.Context) error {
	s.reconcile.mu.Lock()
	defer s.reconcile.mu.Unlock()
	s.initReconcileLocked()
	if s.reconcile.quiesced || s.store == nil || s.provider == nil {
		return ErrPaymentUnavailable
	}
	if err := s.store.CheckMutationAllowed(ctx); err != nil {
		return err
	}
	s.reconcile.active++
	return nil
}

func (s *Service) endPaymentRequest() {
	s.reconcile.mu.Lock()
	s.reconcile.active--
	close(s.reconcile.changed)
	s.reconcile.changed = make(chan struct{})
	s.reconcile.mu.Unlock()
}

// Every scheduled provider operation takes a slot, including recovery Create
// and Cancel. Waiting is outside SQLite and outside the provider's 15s budget.
func (s *Service) providerSlot(ctx context.Context) error {
	s.reconcile.mu.Lock()
	s.initReconcileLocked()
	if s.reconcile.quiesced {
		s.reconcile.mu.Unlock()
		return ErrPaymentUnavailable
	}
	now := time.Now()
	at := s.reconcile.nextRequest
	if at.Before(now) {
		at = now
	}
	s.reconcile.nextRequest = at.Add(time.Second)
	pause := s.reconcile.pause
	s.reconcile.mu.Unlock()
	if wait := time.Until(at); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pause:
			return ErrPaymentUnavailable
		case <-timer.C:
		}
	}
	s.reconcile.mu.Lock()
	paused := s.reconcile.quiesced
	s.reconcile.mu.Unlock()
	if paused {
		return ErrPaymentUnavailable
	}
	return s.store.CheckMutationAllowed(ctx)
}

func (s *Service) reconcileDue(ctx context.Context) error {
	s.reconcile.mu.Lock()
	paused := s.reconcile.quiesced
	s.reconcile.mu.Unlock()
	if paused || s.store == nil || s.provider == nil {
		return nil
	}
	if err := s.store.CheckMutationAllowed(ctx); err != nil {
		return err
	}
	if err := s.store.WakePendingPaymentCallbacks(ctx, s.cfg.PayOSClientID); err != nil {
		return err
	}
	orders, err := s.store.DuePaymentOrders(ctx, s.cfg.PayOSClientID, time.Now(), 20)
	if err != nil {
		return err
	}
	for _, order := range orders {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.reconcileOrder(ctx, order.ID); err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrMutationGateLocked)) {
			return err
		}
	}
	return nil
}

func (s *Service) getClaimedLink(ctx context.Context, order storage.PaymentOrder) (*payos.PaymentLink, error) {
	if err := s.providerSlot(ctx); err != nil {
		return nil, err
	}
	if _, err := s.store.RenewPaymentOrderOperation(ctx, order.ID, order.OperationToken, false); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	link, err := s.provider.Get(requestCtx, order.OrderCode)
	if err == nil && link != nil {
		s.recordActivity(ctx, "RECONCILED")
	}
	return link, err
}

func (s *Service) reconcileOrder(ctx context.Context, id string) error {
	if err := s.beginPaymentRequest(ctx); err != nil {
		return err
	}
	defer s.endPaymentRequest()
	order, claimed, err := s.store.ClaimPaymentOrder(ctx, id, time.Now())
	if err != nil || !claimed {
		return err
	}
	link, err := s.getClaimedLink(ctx, order)
	if err != nil {
		var failure *ProviderError
		if errors.As(err, &failure) && failure.HTTPStatus == http.StatusNotFound {
			return s.recoverAbsent(ctx, order)
		}
		return s.finishReconcile(ctx, order, s.failureUpdate(order, err, "PROVIDER_UNAVAILABLE"))
	}
	return s.applyVerifiedLink(ctx, order, link)
}

func verifiedLinkMatches(order storage.PaymentOrder, link *payos.PaymentLink) bool {
	return link != nil && link.OrderCode == order.OrderCode && int64(link.Amount) == order.AmountVnd && strings.TrimSpace(link.Id) != "" && (order.PaymentLinkID == "" || order.PaymentLinkID == link.Id)
}

func (s *Service) applyVerifiedLink(ctx context.Context, order storage.PaymentOrder, link *payos.PaymentLink) error {
	if !verifiedLinkMatches(order, link) {
		return s.finishReconcile(ctx, order, s.failureUpdate(order, nil, "PAYMENT_MISMATCH"))
	}
	if link.Status == payos.PaymentLinkStatusPaid {
		result, err := s.settleVerifiedLink(ctx, order, link)
		if err == nil && result.ReviewReason == "" {
			s.publishSettlement(result)
			_ = s.finishReconcile(ctx, order, storage.PaymentOrderUpdate{})
			return nil
		}
		if err != nil && !isSettlementReview(err) {
			return s.finishReconcile(ctx, order, s.failureUpdate(order, err, "PAYMENT_DETAILS_PENDING"))
		}
		return s.finishReconcile(ctx, order, s.failureUpdate(order, nil, "PAYMENT_DETAILS_PENDING"))
	}
	update := storage.PaymentOrderUpdate{PaymentLinkID: link.Id, NextReconcileAt: time.Now().UTC().Add(time.Minute), ResetReconcileAttempts: true}
	switch string(link.Status) {
	case "PENDING", "PROCESSING", "UNDERPAID":
		update.Status = string(link.Status)
		if order.LastErrorCode == "CANCEL_OUTCOME_UNKNOWN" {
			return s.reconcileCancellation(ctx, order)
		}
		if order.QRCode == "" && string(link.Status) == "PENDING" {
			order.PaymentLinkID = link.Id
			if order.QRRecoveryAttempted {
				return s.recoverCancel(ctx, order)
			}
			return s.recoverCreate(ctx, order, true)
		}
	case "CANCELLED", "EXPIRED", "FAILED":
		update.Status = string(link.Status)
		if order.QRRecoveryAttempted && order.QRCode == "" && link.Status == payos.PaymentLinkStatusCancelled {
			update.LastErrorCode = "QR_RECOVERY_CANCELLED"
		}
		update.NextReconcileAt = terminalReconcileAt(order)
	default:
		return s.finishReconcile(ctx, order, s.failureUpdate(order, nil, "PAYMENT_DETAILS_PENDING"))
	}
	return s.finishReconcile(ctx, order, update)
}

func terminalReconcileAt(order storage.PaymentOrder) time.Time {
	expires, err := time.Parse(time.RFC3339Nano, order.ExpiresAt)
	if err != nil {
		return time.Now().UTC().Add(24 * time.Hour)
	}
	at := expires.Add(24 * time.Hour)
	if !time.Now().Before(at) {
		return time.Time{}
	}
	return at
}

func (s *Service) recoverAbsent(ctx context.Context, order storage.PaymentOrder) error {
	expires, err := time.Parse(time.RFC3339Nano, order.ExpiresAt)
	if err != nil {
		return err
	}
	if order.QRRecoveryAttempted {
		return s.recoverCancel(ctx, order)
	}
	if order.LastErrorCode == "CANCEL_OUTCOME_UNKNOWN" {
		return s.reconcileCancellation(ctx, order)
	}
	if strings.HasPrefix(order.LastErrorCode, "CREATE_REJECTED_HTTP_") {
		return s.finishReconcile(ctx, order, storage.PaymentOrderUpdate{Status: "FAILED", LastErrorCode: order.LastErrorCode, NextReconcileAt: terminalReconcileAt(order)})
	}
	if time.Now().Before(expires) && (order.Status == "CREATING" || order.LastErrorCode == "CREATE_OUTCOME_UNKNOWN" || order.LastErrorCode == "INVALID_CREATE_RESPONSE") {
		return s.recoverCreate(ctx, order, false)
	}
	return s.finishReconcile(ctx, order, storage.PaymentOrderUpdate{Status: order.Status, LastErrorCode: "CREATE_OUTCOME_UNKNOWN", NextReconcileAt: terminalReconcileAt(order)})
}

func (s *Service) recoverCreate(ctx context.Context, order storage.PaymentOrder, knownLink bool) error {
	if err := s.providerSlot(ctx); err != nil {
		return s.finishReconcile(ctx, order, s.failureUpdate(order, err, "CREATE_OUTCOME_UNKNOWN"))
	}
	expectedLinkID := order.PaymentLinkID
	renewed, err := s.store.RenewPaymentOrderOperation(ctx, order.ID, order.OperationToken, knownLink)
	if err != nil {
		return err
	}
	order = renewed
	if expectedLinkID != "" {
		order.PaymentLinkID = expectedLinkID
	}
	expires, err := time.Parse(time.RFC3339Nano, order.ExpiresAt)
	if err != nil {
		return err
	}
	expiresUnix := int(expires.Unix())
	capabilityURL := s.cfg.PaymentPublicOrigin + "/pay/" + order.ID
	requestCtx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	response, err := s.provider.Create(requestCtx, payos.CreatePaymentLinkRequest{OrderCode: order.OrderCode, Amount: int(order.AmountVnd), Description: order.Description, ReturnUrl: capabilityURL, CancelUrl: capabilityURL, ExpiredAt: &expiresUnix})
	cancel()
	if err == nil && validCreateResponse(order, response) && (order.PaymentLinkID == "" || order.PaymentLinkID == response.PaymentLinkId) {
		return s.finishReconcile(ctx, order, storage.PaymentOrderUpdate{Status: "PENDING", PaymentLinkID: response.PaymentLinkId, QRCode: response.QrCode, CheckoutURL: response.CheckoutUrl, BankBin: response.Bin, AccountNumber: response.AccountNumber, AccountName: response.AccountName, NextReconcileAt: time.Now().UTC().Add(time.Minute), ResetReconcileAttempts: true})
	}
	if knownLink {
		return s.recoverCancel(ctx, order)
	}
	update := s.failureUpdate(order, err, "CREATE_OUTCOME_UNKNOWN")
	var failure *ProviderError
	if errors.As(err, &failure) && !failure.Indeterminate && failure.HTTPStatus >= 400 && failure.HTTPStatus < 500 && failure.HTTPStatus != 408 && failure.HTTPStatus != 429 {
		update.LastErrorCode = "CREATE_REJECTED_HTTP_" + strconv.Itoa(failure.HTTPStatus)
	}
	return s.finishReconcile(ctx, order, update)
}

func (s *Service) recoverCancel(ctx context.Context, order storage.PaymentOrder) error {
	link, cancelErr := s.cancelClaimedLink(ctx, order)
	if cancelErr == nil && verifiedLinkMatches(order, link) && (string(link.Status) == "CANCELLED" || string(link.Status) == "EXPIRED") {
		return s.applyVerifiedLink(ctx, order, link)
	}
	// A Cancel response, timeout or intermediate state cannot prove payment.
	link, getErr := s.getClaimedLink(ctx, order)
	if getErr == nil && verifiedLinkMatches(order, link) && (string(link.Status) == "CANCELLED" || string(link.Status) == "PAID" || string(link.Status) == "EXPIRED") {
		return s.applyVerifiedLink(ctx, order, link)
	}
	update := s.failureUpdate(order, cancelErr, "QR_RECOVERY_CANCEL_PENDING")
	getUpdate := s.failureUpdate(order, getErr, "QR_RECOVERY_CANCEL_PENDING")
	if getUpdate.NextReconcileAt.After(update.NextReconcileAt) {
		update.NextReconcileAt = getUpdate.NextReconcileAt
	}
	return s.finishReconcile(ctx, order, update)
}

func (s *Service) finishReconcile(ctx context.Context, order storage.PaymentOrder, update storage.PaymentOrderUpdate) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.store.CompletePaymentOrderOperation(persistCtx, order.ID, order.OperationToken, update)
	if errors.Is(err, storage.ErrPaymentOperationLost) {
		return nil
	}
	return err
}

func (s *Service) failureUpdate(order storage.PaymentOrder, err error, code string) storage.PaymentOrderUpdate {
	attempt := order.ReconcileAttempts + 1
	delay := 30 * time.Second
	for i := 1; i < attempt && delay < 15*time.Minute; i++ {
		delay *= 2
	}
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	if jitter, randomErr := rand.Int(rand.Reader, big.NewInt(int64(delay/5)+1)); randomErr == nil {
		delay += time.Duration(jitter.Int64())
	}
	var failure *ProviderError
	if errors.As(err, &failure) && failure.RetryAfter > delay {
		delay = failure.RetryAfter
	}
	if code == "PROVIDER_UNAVAILABLE" {
		if strings.HasPrefix(order.LastErrorCode, "CREATE_REJECTED_HTTP_") || order.LastErrorCode == "CANCEL_OUTCOME_UNKNOWN" || order.LastErrorCode == "QR_RECOVERY_CANCEL_PENDING" {
			code = order.LastErrorCode
		}
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, order.ExpiresAt)
	if parseErr == nil && !time.Now().Before(expires.Add(24*time.Hour)) && order.Status == "CREATING" {
		code = "CREATE_OUTCOME_UNKNOWN"
	}
	return storage.PaymentOrderUpdate{LastErrorCode: code, ReconcileAttempts: attempt, NextReconcileAt: time.Now().UTC().Add(delay)}
}

func parsePaymentTransactionTime(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	// Vietnam has no seasonal time changes. A fixed zone works even on hosts
	// without an installed IANA timezone database.
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60))
	return parsed.UTC(), err
}

func effectiveAccount(account, virtual string) string {
	if virtual != "" {
		return virtual
	}
	return account
}
func dereferenceString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func verifiedLinkSettlement(order storage.PaymentOrder, link *payos.PaymentLink) (storage.SettlementInput, error) {
	if !verifiedLinkMatches(order, link) || string(link.Status) != "PAID" || int64(link.AmountPaid) != order.AmountVnd || link.AmountRemaining != 0 || len(link.Transactions) != 1 {
		return storage.SettlementInput{}, &storage.SettlementReviewError{Reason: "PAYMENT_DETAILS_PENDING"}
	}
	transaction := link.Transactions[0]
	at, err := parsePaymentTransactionTime(transaction.TransactionDateTime)
	virtual := dereferenceString(transaction.VirtualAccountNumber)
	if err != nil || strings.TrimSpace(transaction.Reference) == "" || int64(transaction.Amount) != order.AmountVnd || strings.TrimSpace(effectiveAccount(transaction.AccountNumber, virtual)) == "" {
		return storage.SettlementInput{}, &storage.SettlementReviewError{Reason: "PAYMENT_DETAILS_PENDING"}
	}
	input := storage.SettlementInput{ChannelID: order.ChannelID, OrderCode: order.OrderCode, PaymentLinkID: link.Id, Reference: transaction.Reference, AmountVnd: int64(transaction.Amount), TransactionAt: at, Description: transaction.Description, AccountNumber: transaction.AccountNumber, VirtualAccountNumber: virtual, Source: "CATCH_UP"}
	input.VerifiedGet = &storage.SettlementEvidence{OrderCode: order.OrderCode, PaymentLinkID: link.Id, Reference: input.Reference, AmountVnd: input.AmountVnd, TransactionAt: at, AccountNumber: transaction.AccountNumber, VirtualAccountNumber: virtual}
	return input, nil
}

// settleVerifiedLink only accepts SDK-signature-verified Get data. It never
// fabricates a reference, substitutes a date, or sums abnormal partial transfers.
// The caller owns post-commit publication.
func (s *Service) settleVerifiedLink(ctx context.Context, order storage.PaymentOrder, link *payos.PaymentLink) (storage.SettlementResult, error) {
	input, err := verifiedLinkSettlement(order, link)
	if err != nil {
		return storage.SettlementResult{}, err
	}
	input.ChannelNamespace = s.channelNamespace
	items, err := s.store.PendingPaymentCallbacks(ctx, order.OrderCode)
	if err != nil {
		return storage.SettlementResult{}, err
	}
	var result storage.SettlementResult
	var event *storage.EventNotification
	matched := false
	for _, item := range items {
		callback, decodeErr := decodeStoredCallback(item)
		if decodeErr != nil || !callbackMatchesSettlement(callback, input) {
			continue
		}
		matched = true
		input.InboxHash = item.PayloadHash
		result, err = s.store.SettlePayment(ctx, input)
		if err != nil || result.ReviewReason != "" {
			return result, err
		}
		if result.Event != nil {
			event = result.Event
		}
	}
	result.Event = event
	if matched {
		return result, nil
	}
	if len(items) > 0 {
		return storage.SettlementResult{}, &storage.SettlementReviewError{Reason: "PAYMENT_DETAILS_PENDING"}
	}
	return s.store.SettlePayment(ctx, input)
}

func decodeStoredCallback(item storage.PaymentInboxItem) (*payos.WebhookData, error) {
	var callback payos.WebhookData
	err := json.Unmarshal([]byte(item.PayloadJSON), &callback)
	return &callback, err
}

func callbackMatchesSettlement(callback *payos.WebhookData, input storage.SettlementInput) bool {
	at, err := parsePaymentTransactionTime(callback.TransactionDateTime)
	return err == nil && callback.Code == "00" && callback.Currency == "VND" && callback.OrderCode == input.OrderCode && callback.PaymentLinkId == input.PaymentLinkID && callback.Reference == input.Reference && int64(callback.Amount) == input.AmountVnd && at.Equal(input.TransactionAt) && effectiveAccount(callback.AccountNumber, dereferenceString(callback.VirtualAccountNumber)) == effectiveAccount(input.AccountNumber, input.VirtualAccountNumber)
}

func isSettlementReview(err error) bool {
	var review *storage.SettlementReviewError
	return errors.As(err, &review)
}
func (s *Service) publishSettlement(result storage.SettlementResult) {
	if result.Event != nil && s.onCommit != nil {
		s.onCommit(*result.Event)
	}
}

func (s *Service) confirmWebhookPayment(ctx context.Context, order storage.PaymentOrder, data *payos.WebhookData, input storage.SettlementInput) (storage.SettlementResult, error) {
	if err := s.beginPaymentRequest(ctx); err != nil {
		return storage.SettlementResult{}, err
	}
	defer s.endPaymentRequest()
	claimedOrder, claimed, err := s.store.ClaimPaymentOrder(ctx, order.ID, time.Now())
	if err != nil {
		return storage.SettlementResult{}, err
	}
	if claimedOrder.Status == "PAID" {
		return s.store.SettlePayment(ctx, input)
	}
	if !claimed {
		return storage.SettlementResult{}, ErrPaymentUnavailable
	}
	order = claimedOrder
	link, err := s.getClaimedLink(ctx, order)
	if err != nil {
		_ = s.finishReconcile(ctx, order, s.failureUpdate(order, err, "PAYMENT_DETAILS_PENDING"))
		return storage.SettlementResult{}, err
	}
	verified, err := verifiedLinkSettlement(order, link)
	if err != nil || !callbackMatchesSettlement(data, verified) {
		if input.VerifiedCallback != nil {
			callback := *input.VerifiedCallback
			callback.Reason = "PAYMENT_DETAILS_PENDING"
			hash, saveErr := s.store.SaveVerifiedPaymentCallback(ctx, callback)
			if saveErr == nil {
				_ = s.finishReconcile(ctx, order, s.failureUpdate(order, nil, "PAYMENT_DETAILS_PENDING"))
			}
			return storage.SettlementResult{Order: order, ReviewReason: callback.Reason, InboxHash: hash}, saveErr
		}
		return storage.SettlementResult{}, &storage.SettlementReviewError{Reason: "PAYMENT_DETAILS_PENDING"}
	}
	input.VerifiedGet = verified.VerifiedGet
	result, err := s.store.SettlePayment(ctx, input)
	if err == nil {
		_ = s.finishReconcile(ctx, order, storage.PaymentOrderUpdate{})
	}
	return result, err
}
