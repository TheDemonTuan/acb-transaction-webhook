package payments

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

var ErrPaymentAlreadyPaid = &ServiceError{Code: "PAYMENT_ALREADY_PAID", HTTPStatus: http.StatusConflict}
var ErrPaymentNotFound = &ServiceError{Code: "PAYMENT_NOT_FOUND", HTTPStatus: http.StatusNotFound}

// CancelOrder does not consult new-order operational flags: an issued order must
// remain cancellable/reconcilable while new payments are disabled.
func (s *Service) CancelOrder(ctx context.Context, id string) (storage.PaymentOrder, error) {
	if s.managed {
		operation, operationCtx, done, err := s.pin(ctx)
		if err != nil {
			return storage.PaymentOrder{}, err
		}
		defer done()
		return operation.CancelOrder(operationCtx, id)
	}
	if s.store == nil {
		return storage.PaymentOrder{}, ErrPaymentUnavailable
	}
	order, err := s.store.PaymentOrder(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && order.ChannelID != s.cfg.PayOSClientID) {
		return storage.PaymentOrder{}, ErrPaymentNotFound
	}
	if err != nil {
		return storage.PaymentOrder{}, ErrPaymentUnavailable
	}
	if order.Status == "PAID" {
		return order, ErrPaymentAlreadyPaid
	}
	if err = s.beginPaymentRequest(ctx); err != nil {
		return order, err
	}
	defer s.endPaymentRequest()
	order, claimed, err := s.store.ClaimPaymentOrder(ctx, id, time.Now())
	if err != nil {
		return order, err
	}
	if order.Status == "PAID" {
		return order, ErrPaymentAlreadyPaid
	}
	if !claimed {
		return order, nil
	}
	link, cancelErr := s.cancelClaimedLink(ctx, order)
	if cancelErr == nil && verifiedLinkMatches(order, link) && (string(link.Status) == "CANCELLED" || string(link.Status) == "EXPIRED") {
		err = s.completeCancellation(ctx, order, link)
	} else {
		// A timeout or ambiguous Cancel cannot manufacture CANCELLED. A signed
		// Get may instead establish a payment that won the cancellation race.
		link, getErr := s.getClaimedLink(ctx, order)
		if getErr == nil && verifiedLinkMatches(order, link) && (string(link.Status) == "PAID" || string(link.Status) == "CANCELLED" || string(link.Status) == "EXPIRED") {
			err = s.completeCancellation(ctx, order, link)
		} else {
			update := s.failureUpdate(order, cancelErr, "CANCEL_OUTCOME_UNKNOWN")
			getUpdate := s.failureUpdate(order, getErr, "CANCEL_OUTCOME_UNKNOWN")
			if getUpdate.NextReconcileAt.After(update.NextReconcileAt) {
				update.NextReconcileAt = getUpdate.NextReconcileAt
			}
			err = s.finishReconcile(ctx, order, update)
		}
	}
	if err != nil {
		return order, err
	}
	latest, err := s.store.PaymentOrder(context.WithoutCancel(ctx), id)
	if err != nil {
		return order, ErrPaymentUnavailable
	}
	if latest.Status == "PAID" {
		return latest, ErrPaymentAlreadyPaid
	}
	return latest, nil
}

func (s *Service) cancelClaimedLink(ctx context.Context, order storage.PaymentOrder) (*payos.PaymentLink, error) {
	if err := s.providerSlot(ctx); err != nil {
		return nil, err
	}
	if _, err := s.store.RenewPaymentOrderOperation(ctx, order.ID, order.OperationToken, false); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	return s.provider.Cancel(requestCtx, order.OrderCode)
}

func (s *Service) completeCancellation(ctx context.Context, order storage.PaymentOrder, link *payos.PaymentLink) error {
	if string(link.Status) == "PAID" {
		result, err := s.settleVerifiedLink(ctx, order, link)
		if err != nil || result.ReviewReason != "" {
			if err != nil && !isSettlementReview(err) {
				return err
			}
			return s.finishReconcile(ctx, order, s.failureUpdate(order, nil, "PAYMENT_DETAILS_PENDING"))
		}
		s.publishSettlement(result)
		_ = s.finishReconcile(ctx, order, storage.PaymentOrderUpdate{})
		return nil
	}
	return s.finishReconcile(ctx, order, storage.PaymentOrderUpdate{Status: string(link.Status), PaymentLinkID: link.Id, NextReconcileAt: terminalReconcileAt(order), ResetReconcileAttempts: true})
}

func (s *Service) reconcileCancellation(ctx context.Context, order storage.PaymentOrder) error {
	link, err := s.cancelClaimedLink(ctx, order)
	if err == nil && verifiedLinkMatches(order, link) && (string(link.Status) == "CANCELLED" || string(link.Status) == "EXPIRED") {
		return s.completeCancellation(ctx, order, link)
	}
	link, getErr := s.getClaimedLink(ctx, order)
	if getErr == nil && verifiedLinkMatches(order, link) && (string(link.Status) == "PAID" || string(link.Status) == "CANCELLED" || string(link.Status) == "EXPIRED") {
		return s.completeCancellation(ctx, order, link)
	}
	update := s.failureUpdate(order, err, "CANCEL_OUTCOME_UNKNOWN")
	getUpdate := s.failureUpdate(order, getErr, "CANCEL_OUTCOME_UNKNOWN")
	if getUpdate.NextReconcileAt.After(update.NextReconcileAt) {
		update.NextReconcileAt = getUpdate.NextReconcileAt
	}
	return s.finishReconcile(ctx, order, update)
}
