package httpapi

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type PaymentActivationRequest struct {
	Identifier string `json:"identifier,omitempty"`
}

type PaymentActivationResponse struct {
	TrackingActive bool   `json:"trackingActive"`
	Phase          string `json:"phase,omitempty"`
}

type clientBucket struct {
	tokens     float64
	lastRefill time.Time
}

type activationLimiter struct {
	mu      sync.Mutex
	buckets map[string]*clientBucket
}

func newActivationLimiter() *activationLimiter {
	return &activationLimiter{
		buckets: make(map[string]*clientBucket),
	}
}

func (l *activationLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	// Periodic cleanup of stale entries if map gets large
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
			tokens:     1, // Allow burst up to 2, starts with 1 remaining after initial
			lastRefill: now,
		}
		return true
	}

	// Refill 1 token every 5 seconds (0.2 tokens / sec), max 2 tokens
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * 0.2
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

type activationDebouncer struct {
	mu         sync.Mutex
	lastCallAt time.Time
}

func (d *activationDebouncer) ShouldCall() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	if !d.lastCallAt.IsZero() && now.Sub(d.lastCallAt) < 2*time.Second {
		return false
	}
	d.lastCallAt = now
	return true
}

func isSafeIdentifier(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
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
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
		return
	}

	ip := getClientIP(r)
	if s.activationLimit != nil && !s.activationLimit.Allow(ip) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "too many requests"})
		return
	}

	// Validate request body if provided
	if r.Body != nil && r.ContentLength != 0 {
		var req PaymentActivationRequest
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil && err != io.EOF {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "malformed payload"})
			return
		}

		if req.Identifier != "" {
			trimmed := strings.TrimSpace(req.Identifier)
			if len(trimmed) < 8 || len(trimmed) > 128 || !isSafeIdentifier(trimmed) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid identifier"})
				return
			}
		}
	}

	if s.paymentActivator == nil {
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{TrackingActive: false})
		return
	}

	// Application-level collapse: activations within 2s do not flood worker RPC
	if s.activationDebounce != nil && !s.activationDebounce.ShouldCall() {
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
			TrackingActive: true,
			Phase:          "GRACE",
		})
		return
	}

	if err := s.paymentActivator.ActivatePaymentWindow(r.Context()); err != nil {
		_ = json.NewEncoder(w).Encode(PaymentActivationResponse{TrackingActive: false})
		return
	}

	_ = json.NewEncoder(w).Encode(PaymentActivationResponse{
		TrackingActive: true,
		Phase:          "GRACE",
	})
}
