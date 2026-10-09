package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
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
	Bank            string `json:"bank"`
	Provider        string `json:"provider,omitempty"`
	OrderCode       string `json:"orderCode,omitempty"`
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
		Bank:            t.Bank,
		Provider:        t.Provider,
		OrderCode:       t.OrderCode,
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
	// Public endpoint strictly isolates and exposes ONLY credit (incoming) transactions.
	// Debit (outgoing) transactions are filtered out and prohibited.
	direction := "credit"
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
	var sanitizedSummary *storage.TransactionSummary
	if page.Summary != nil {
		sanitizedSummary = &storage.TransactionSummary{
			TotalCount: page.Summary.TotalCount,
			Incoming:   page.Summary.Incoming,
			Outgoing:   0,
		}
	}
	writeJSON(w, http.StatusOK, publicTransactionsPage{
		Items:      publicItems,
		NextCursor: page.NextCursor,
		Summary:    sanitizedSummary,
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
	// Public viewers are strictly forbidden from viewing debit (money out) transactions
	if txn.Debit > 0 || txn.Credit == 0 {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, toPublicTransaction(*txn))
}
