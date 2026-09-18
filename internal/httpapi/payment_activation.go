package httpapi

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type PaymentActivationRequest struct {
	Identifier string `json:"identifier"`
}

type PaymentActivationResponse struct {
	TrackingActive bool   `json:"trackingActive"`
	Phase          string `json:"phase,omitempty"`
	NextPhaseAt    string `json:"nextPhaseAt,omitempty"`
	Code           string `json:"code,omitempty"`
	Error          string `json:"error,omitempty"`
}

type clientBucket struct {
	tokens     float64
	lastRefill time.Time
}

type identifierTracker struct {
	count       int
	windowStart time.Time
}

type activationLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	buckets     map[string]*clientBucket
	identifiers map[string]*identifierTracker
}

func newActivationLimiter() *activationLimiter {
	return &activationLimiter{
		now:         time.Now,
		buckets:     make(map[string]*clientBucket),
		identifiers: make(map[string]*identifierTracker),
	}
}

func (l *activationLimiter) AllowIP(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if len(l.buckets) > 5000 {
		for k, b := range l.buckets {
			if now.Sub(b.lastRefill) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}

	b, exists := l.buckets[ip]
	if !exists {
		l.buckets[ip] = &clientBucket{
			tokens:     1, // burst 2 (1 token consumed below leaves 1)
			lastRefill: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * 0.2 // 1 token every 5s
	if b.tokens > 2.0 {
		b.tokens = 2.0
	}
	b.lastRefill = now

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true
	}
	return false
}

// AllowIdentifier bounds activations per identifier (max 6 per 15 minutes).
func (l *activationLimiter) AllowIdentifier(identifier string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if len(l.identifiers) > 5000 {
		for k, tracker := range l.identifiers {
			if now.Sub(tracker.windowStart) > 30*time.Minute {
				delete(l.identifiers, k)
			}
		}
	}

	tracker, exists := l.identifiers[identifier]
	if !exists || now.Sub(tracker.windowStart) >= 15*time.Minute {
		l.identifiers[identifier] = &identifierTracker{
			count:       1,
			windowStart: now,
		}
		return true
	}

	if tracker.count >= 6 {
		return false
	}
	tracker.count++
	return true
}

type debounceEntry struct {
	state   PaymentActivationState
	err     error
	savedAt time.Time
}

type activationDebouncer struct {
	mu      sync.Mutex
	now     func() time.Time
	last    *debounceEntry
	window  time.Duration
}

func newActivationDebouncer() *activationDebouncer {
	return &activationDebouncer{
		now:    time.Now,
		window: 2 * time.Second,
	}
}

func (d *activationDebouncer) Cached() (PaymentActivationState, error, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.last == nil {
		return PaymentActivationState{}, nil, false
	}
	now := d.now()
	if now.Sub(d.last.savedAt) < d.window {
		return d.last.state, d.last.err, true
	}
	return PaymentActivationState{}, nil, false
}

func (d *activationDebouncer) Commit(state PaymentActivationState, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last = &debounceEntry{
		state:   state,
		err:     err,
		savedAt: d.now(),
	}
}

func isSafeIdentifier(s string) bool {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 8 || len(trimmed) > 128 {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func getClientIP(r *http.Request) string {
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		return cf
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) activatePaymentQR(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "METHOD_NOT_ALLOWED",
			Error:          "method not allowed",
		})
		return
	}

	ip := getClientIP(r)
	if s.activationLimit != nil && !s.activationLimit.AllowIP(ip) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "RATE_LIMITED",
			Error:          "too many requests",
		})
		return
	}

	// Always require and decode a JSON body. Never allow blank {} or missing identifier.
	// We read up to 4096 bytes regardless of Content-Length to close chunked-transfer bypasses.
	if r.Body == nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "IDENTIFIER_REQUIRED",
			Error:          "identifier required",
		})
		return
	}

	var req PaymentActivationRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "MALFORMED_BODY",
			Error:          "malformed activation request",
		})
		return
	}

	identifier := strings.TrimSpace(req.Identifier)
	if identifier == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "IDENTIFIER_REQUIRED",
			Error:          "identifier required",
		})
		return
	}

	if !isSafeIdentifier(identifier) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "INVALID_IDENTIFIER",
			Error:          "invalid identifier",
		})
		return
	}

	if s.activationLimit != nil && !s.activationLimit.AllowIdentifier(identifier) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "IDENTIFIER_RATE_LIMITED",
			Error:          "too many activations for identifier",
		})
		return
	}

	if s.paymentActivator == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "ACTIVATION_UNAVAILABLE",
			Error:          "activation service unavailable",
		})
		return
	}

	// Application-level debouncing with honest replay:
	// If a recent call was completed within 2s, replay the EXACT cached state or error.
	var state PaymentActivationState
	var rpcErr error
	if s.activationDebounce != nil {
		if cachedState, cachedErr, ok := s.activationDebounce.Cached(); ok {
			state = cachedState
			rpcErr = cachedErr
			s.writeActivationResponse(w, identifier, state, rpcErr)
			return
		}
	}

	state, rpcErr = s.paymentActivator.ActivatePaymentWindow(r.Context())
	if s.activationDebounce != nil {
		s.activationDebounce.Commit(state, rpcErr)
	}

	s.writeActivationResponse(w, identifier, state, rpcErr)
}

func (s *Server) writeActivationResponse(w http.ResponseWriter, identifier string, state PaymentActivationState, err error) {
	if err != nil {
		slog.Warn("payment activation failed", "identifier", identifier, "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Code:           "ACTIVATION_UNAVAILABLE",
			Error:          "activation failed",
		})
		return
	}

	var nextPhaseStr string
	if !state.NextPhaseAt.IsZero() {
		nextPhaseStr = state.NextPhaseAt.UTC().Format(time.RFC3339)
	}

	if state.Phase == "LOCKED" {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: false,
			Phase:          "LOCKED",
			NextPhaseAt:    nextPhaseStr,
			Code:           "BOOST_BUDGET_EXHAUSTED",
			Error:          "boost budget exhausted",
		})
		return
	}

	slog.Info("payment activation accepted", "identifier", identifier, "phase", state.Phase)
	s.publishStateEvent("payment.activated", identifier, map[string]any{
		"identifier":     identifier,
		"phase":          state.Phase,
		"trackingActive": state.TrackingActive,
		"nextPhaseAt":    nextPhaseStr,
		"activatedAt":    time.Now().UTC().Format(time.RFC3339),
	})
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
		TrackingActive: state.TrackingActive,
		Phase:          state.Phase,
		NextPhaseAt:    nextPhaseStr,
	})
}
