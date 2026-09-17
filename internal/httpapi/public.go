package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type publicTransaction struct {
	ID              string `json:"id"`
	SemanticKey     string `json:"semanticKey"`
	TransactionDate string `json:"transactionDate"`
	TransactionDay  string `json:"transactionDay,omitempty"`
	DatePrecision   string `json:"datePrecision,omitempty"`
	EffectiveDate   string `json:"effectiveDate"`
	Debit           int64  `json:"debit"`
	Credit          int64  `json:"credit"`
	Description     string `json:"description"`
	FirstSeenAt     string `json:"firstSeenAt"`
	Source          string `json:"source,omitempty"`
}

type publicTransactionsPage struct {
	Items      []publicTransaction         `json:"items"`
	NextCursor string                      `json:"nextCursor,omitempty"`
	Summary    *storage.TransactionSummary `json:"summary,omitempty"`
}

func toPublicTransaction(t storage.TransactionView) publicTransaction {
	return publicTransaction{
		ID:              t.ID,
		SemanticKey:     t.SemanticKey,
		TransactionDate: t.TransactionAt,
		TransactionDay:  t.TransactionDay,
		DatePrecision:   t.DatePrecision,
		EffectiveDate:   t.EffectiveAt,
		Debit:           t.Debit,
		Credit:          t.Credit,
		Description:     t.Description,
		FirstSeenAt:     t.FirstSeenAt,
		Source:          t.Source,
	}
}

func (s *Server) publicTransactions(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		q = q[:200]
	}
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	direction := strings.TrimSpace(r.URL.Query().Get("direction"))
	if direction != "credit" && direction != "debit" {
		direction = "all"
	}
	if from != "" {
		if _, err := time.Parse("2006-01-02", from); err != nil {
			writeError(w, http.StatusBadRequest, "invalid from date: expected YYYY-MM-DD")
			return
		}
	}
	if to != "" {
		if _, err := time.Parse("2006-01-02", to); err != nil {
			writeError(w, http.StatusBadRequest, "invalid to date: expected YYYY-MM-DD")
			return
		}
	}
	if from != "" && to != "" && from > to {
		writeError(w, http.StatusBadRequest, "from date must not be after to date")
		return
	}

	filter := storage.TransactionFilter{
		From:      from,
		To:        to,
		Direction: direction,
		Query:     q,
		Limit:     limit,
		Cursor:    cursor,
	}
	page, err := s.store.ListTransactionsFiltered(r.Context(), filter)
	if err != nil {
		status := http.StatusInternalServerError
		if cursor != "" {
			status = http.StatusBadRequest
		}
		writeError(w, status, "query transactions failed")
		return
	}

	publicItems := make([]publicTransaction, len(page.Items))
	for i, it := range page.Items {
		publicItems[i] = toPublicTransaction(it)
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, publicTransactionsPage{
		Items:      publicItems,
		NextCursor: page.NextCursor,
		Summary:    page.Summary,
	})
}

func (s *Server) publicTransactionDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "transaction id is required")
		return
	}
	txn, err := s.store.GetTransactionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get transaction")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, toPublicTransaction(*txn))
}

func (s *Server) publicPaymentQR(w http.ResponseWriter, r *http.Request) {
	qr, err := s.store.GetPaymentQR(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get payment qr")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if qr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"hasImage":   qr.ImagePath != "",
		"imageURL":   "/api/public/v1/payment-qr/image?v=" + strconv.FormatInt(qr.Revision, 10),
		"qr": map[string]any{
			"accountName":   qr.AccountName,
			"accountNumber": qr.AccountNumber,
			"bankName":      qr.BankName,
		},
	})
}
