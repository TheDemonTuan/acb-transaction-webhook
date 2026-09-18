package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// CanaryHit stores telemetry details for a single canary request.
type CanaryHit struct {
	Timestamp time.Time `json:"timestamp"`
	SourceIP  string    `json:"sourceIp"`
	UserAgent string    `json:"userAgent"`
	Referer   string    `json:"referer,omitempty"`
}

// CanaryTokenStatus tracks status and history of hits for a specific canary test token.
type CanaryTokenStatus struct {
	Token         string      `json:"token"`
	CreatedAt     time.Time   `json:"createdAt"`
	HitCount      int         `json:"hitCount"`
	FirstHitAt    *time.Time  `json:"firstHitAt,omitempty"`
	LastHitAt     *time.Time  `json:"lastHitAt,omitempty"`
	LastUserAgent string      `json:"lastUserAgent,omitempty"`
	LastSourceIP  string      `json:"lastSourceIp,omitempty"`
	RecentHits    []CanaryHit `json:"recentHits,omitempty"`
}

// CanaryTracker provides thread-safe in-memory tracking of canary scan events.
type CanaryTracker struct {
	mu       sync.RWMutex
	tokens   map[string]*CanaryTokenStatus
	ipLimits map[string][]time.Time
}

func newCanaryTracker() *CanaryTracker {
	return &CanaryTracker{
		tokens:   make(map[string]*CanaryTokenStatus),
		ipLimits: make(map[string][]time.Time),
	}
}

// RegisterToken ensures a token is registered in the tracker.
func (ct *CanaryTracker) RegisterToken(token string) *CanaryTokenStatus {
	clean := strings.TrimSpace(token)
	if clean == "" {
		return nil
	}
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Bound memory size to 1,000 active tokens
	if len(ct.tokens) >= 1000 {
		var oldestToken string
		var oldestTime time.Time
		for t, st := range ct.tokens {
			if oldestTime.IsZero() || st.CreatedAt.Before(oldestTime) {
				oldestTime = st.CreatedAt
				oldestToken = t
			}
		}
		if oldestToken != "" {
			delete(ct.tokens, oldestToken)
		}
	}

	st, exists := ct.tokens[clean]
	if !exists {
		st = &CanaryTokenStatus{
			Token:      clean,
			CreatedAt:  time.Now().UTC(),
			RecentHits: make([]CanaryHit, 0),
		}
		ct.tokens[clean] = st
	}
	return st
}

// AllowIP applies rate limiting per client IP (max 30 requests per minute).
func (ct *CanaryTracker) AllowIP(ip string) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-1 * time.Minute)

	history := ct.ipLimits[ip]
	valid := history[:0]
	for _, t := range history {
		if t.After(windowStart) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= 30 {
		ct.ipLimits[ip] = valid
		return false
	}

	ct.ipLimits[ip] = append(valid, now)
	return true
}

// RecordHit records a scan hit for the specified token.
func (ct *CanaryTracker) RecordHit(token string, hit CanaryHit) *CanaryTokenStatus {
	clean := strings.TrimSpace(token)
	ct.mu.Lock()
	defer ct.mu.Unlock()

	st, exists := ct.tokens[clean]
	if !exists {
		st = &CanaryTokenStatus{
			Token:      clean,
			CreatedAt:  hit.Timestamp,
			RecentHits: make([]CanaryHit, 0),
		}
		ct.tokens[clean] = st
	}

	st.HitCount++
	if st.FirstHitAt == nil {
		t := hit.Timestamp
		st.FirstHitAt = &t
	}
	t := hit.Timestamp
	st.LastHitAt = &t
	st.LastUserAgent = hit.UserAgent
	st.LastSourceIP = hit.SourceIP

	// Keep up to 20 recent hits
	st.RecentHits = append(st.RecentHits, hit)
	if len(st.RecentHits) > 20 {
		st.RecentHits = st.RecentHits[len(st.RecentHits)-20:]
	}

	return st
}

// GetStatus returns the current status for the given token.
func (ct *CanaryTracker) GetStatus(token string) (*CanaryTokenStatus, bool) {
	clean := strings.TrimSpace(token)
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	st, exists := ct.tokens[clean]
	if !exists {
		return nil, false
	}

	// Make a shallow copy to return safely
	copySt := *st
	copySt.RecentHits = make([]CanaryHit, len(st.RecentHits))
	copy(copySt.RecentHits, st.RecentHits)
	return &copySt, true
}

// ResetToken clears recorded hits for a token.
func (ct *CanaryTracker) ResetToken(token string) {
	clean := strings.TrimSpace(token)
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if st, exists := ct.tokens[clean]; exists {
		st.HitCount = 0
		st.FirstHitAt = nil
		st.LastHitAt = nil
		st.LastUserAgent = ""
		st.LastSourceIP = ""
		st.RecentHits = make([]CanaryHit, 0)
	}
}

// handlePaymentQRCanary handles public GET /api/public/v1/payment-qr/canary/{token}
func (s *Server) handlePaymentQRCanary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")

	token := strings.TrimSpace(chi.URLParam(r, "token"))
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	// Validate token format (safe identifier)
	if !isSafeIdentifier(token) {
		writeError(w, http.StatusBadRequest, "invalid canary token format")
		return
	}

	ip := getClientIP(r)
	if s.canaryTracker != nil && !s.canaryTracker.AllowIP(ip) {
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	now := time.Now().UTC()
	ua := r.UserAgent()
	ref := r.Referer()

	hit := CanaryHit{
		Timestamp: now,
		SourceIP:  ip,
		UserAgent: ua,
		Referer:   ref,
	}

	log.Printf("[PAYMENT_QR_CANARY_HIT] timestamp=%s token=%s ip=%s ua=%q ref=%q",
		now.Format(time.RFC3339), token, ip, ua, ref)

	var status *CanaryTokenStatus
	if s.canaryTracker != nil {
		status = s.canaryTracker.RecordHit(token, hit)
	}

	// If payment window activator is available, trigger window boost
	if s.paymentActivator != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.paymentActivator.ActivatePaymentWindow(ctx)
		}()
	}

	resp := map[string]any{
		"status":    "recorded",
		"token":     token,
		"timestamp": now.Format(time.RFC3339),
	}
	if status != nil {
		resp["hitCount"] = status.HitCount
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// getCanaryStatus handles admin GET /api/v1/payment-qr/canary/{token}
func (s *Server) getCanaryStatus(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(chi.URLParam(r, "token"))
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	if s.canaryTracker == nil {
		writeError(w, http.StatusServiceUnavailable, "canary tracker not initialized")
		return
	}

	status, found := s.canaryTracker.GetStatus(token)
	if !found {
		// Return empty status if registered implicitly or not yet seen
		writeJSON(w, http.StatusOK, map[string]any{
			"token":     token,
			"hitCount":  0,
			"createdAt": time.Now().UTC(),
		})
		return
	}

	writeJSON(w, http.StatusOK, status)
}

// resetCanaryStatus handles admin DELETE /api/v1/payment-qr/canary/{token}
func (s *Server) resetCanaryStatus(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(chi.URLParam(r, "token"))
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	if s.canaryTracker != nil {
		s.canaryTracker.ResetToken(token)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "reset",
		"token":  token,
	})
}
