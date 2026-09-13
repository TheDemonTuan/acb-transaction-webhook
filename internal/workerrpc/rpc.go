package workerrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	HeaderInternalToken = "X-Worker-Internal-Token"
	HeaderActorID       = "X-Actor-Id"
	HeaderRequestID     = "X-Request-Id"
	HeaderIdempotency   = "Idempotency-Key"
)

type ctxKey string

const requestIDCtxKey ctxKey = "workerrpc.requestId"

func WithRequestID(ctx context.Context, reqID string) context.Context {
	return context.WithValue(ctx, requestIDCtxKey, reqID)
}

func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDCtxKey).(string); ok {
		return v
	}
	return ""
}

func validateOrGenerateRequestID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" && len(trimmed) <= 128 && isSafeRequestID(trimmed) {
		return trimmed
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isSafeRequestID(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}

type ErrorResponse struct {
	Error     string `json:"error"`
	RequestID string `json:"requestId,omitempty"`
}

// EnsureHistoryRequest payload for historical backfill
type EnsureHistoryRequest struct {
	FromDay string `json:"fromDay"`
	ToDay   string `json:"toDay"`
}

type EnsureHistoryResponse struct {
	Count int    `json:"count"`
	Error string `json:"error,omitempty"`
}

// VerifySessionRequest payload for verifying credentials with bank
type VerifySessionRequest struct {
	Account    string `json:"account"`
	Generation int64  `json:"generation"`
	Password   []byte `json:"password"`
}

// Handler interface implemented by worker
type WorkerHandler interface {
	RequestSync(ctx context.Context) error
	EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error)
	NotifySettingsChanged(ctx context.Context) error
	WakeDispatcher(ctx context.Context) error
	VerifySession(ctx context.Context, account string, generation int64, password []byte) error
}

type ServerOption func(*Server)

func WithMaxConcurrent(n int) ServerOption {
	return func(s *Server) {
		if n > 0 {
			s.sem = make(chan struct{}, n)
		}
	}
}

func WithMaxBodyBytes(n int64) ServerOption {
	return func(s *Server) {
		if n > 0 {
			s.maxBodyBytes = n
		}
	}
}

func WithServerTimeout(d time.Duration) ServerOption {
	return func(s *Server) {
		if d > 0 {
			s.serverTimeout = d
		}
	}
}

// Server serves private RPC requests from gateway slots
type Server struct {
	handler       WorkerHandler
	token         string
	mux           *http.ServeMux
	maxBodyBytes  int64
	serverTimeout time.Duration
	sem           chan struct{}

	mu           sync.Mutex
	readyChecker func(ctx context.Context) error
}

func NewValidatedServer(handler WorkerHandler, token string, opts ...ServerOption) (*Server, error) {
	if handler == nil {
		return nil, errors.New("worker rpc handler is required")
	}
	trimmedToken := strings.TrimSpace(token)
	if trimmedToken == "" {
		return nil, errors.New("worker rpc internal token is required")
	}
	s := &Server{
		handler:       handler,
		token:         trimmedToken,
		mux:           http.NewServeMux(),
		maxBodyBytes:  1 << 20, // 1MB
		serverTimeout: 120 * time.Second,
		sem:           make(chan struct{}, 32),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s, nil
}

func NewServer(handler WorkerHandler, token string, opts ...ServerOption) (*Server, error) {
	return NewValidatedServer(handler, token, opts...)
}

func (s *Server) SetReadyChecker(checker func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyChecker = checker
}

func (s *Server) Handler() http.Handler {
	return http.TimeoutHandler(s.mux, s.serverTimeout, `{"error":"server timeout"}`)
}

func writeError(w http.ResponseWriter, status int, msg, reqID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Error:     msg,
		RequestID: reqID,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := validateOrGenerateRequestID(r.Header.Get(HeaderRequestID))
		w.Header().Set(HeaderRequestID, reqID)

		// Bounded concurrency check
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			writeError(w, http.StatusTooManyRequests, "too many concurrent requests", reqID)
			return
		}

		// Constant-time compare fail-closed
		reqToken := strings.TrimSpace(r.Header.Get(HeaderInternalToken))
		if s.token == "" || subtle.ConstantTimeCompare([]byte(reqToken), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", reqID)
			return
		}

		// Max body enforcement
		if r.Body != nil && s.maxBodyBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
		}

		next(w, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		reqID := validateOrGenerateRequestID(r.Header.Get(HeaderRequestID))
		w.Header().Set(HeaderRequestID, reqID)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "requestId": reqID})
	})

	s.mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		reqID := validateOrGenerateRequestID(r.Header.Get(HeaderRequestID))
		w.Header().Set(HeaderRequestID, reqID)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		s.mu.Lock()
		checker := s.readyChecker
		s.mu.Unlock()
		if checker != nil {
			if err := checker(r.Context()); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"status":    "not_ready",
					"error":     err.Error(),
					"requestId": reqID,
				})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "ready",
			"requestId": reqID,
		})
	})

	s.mux.HandleFunc("/rpc/request-sync", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		if err := s.handler.RequestSync(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "requestId": reqID})
	}))

	s.mux.HandleFunc("/rpc/ensure-history", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		var req EnsureHistoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad request", reqID)
			return
		}
		count, err := s.handler.EnsureHistory(r.Context(), req.FromDay, req.ToDay)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, EnsureHistoryResponse{Count: count})
	}))

	s.mux.HandleFunc("/rpc/notify-settings-changed", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		if err := s.handler.NotifySettingsChanged(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "requestId": reqID})
	}))

	s.mux.HandleFunc("/rpc/wake-dispatcher", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		if err := s.handler.WakeDispatcher(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "requestId": reqID})
	}))

	s.mux.HandleFunc("/rpc/verify-session", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		var req VerifySessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad request", reqID)
			return
		}
		if req.Generation <= 0 || req.Account == "" || len(req.Password) == 0 {
			writeError(w, http.StatusBadRequest, "invalid session verification parameters", reqID)
			return
		}
		if err := s.handler.VerifySession(r.Context(), req.Account, req.Generation, req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "requestId": reqID})
	}))
}

// Client calls the worker RPC server from gateway slots
type Client struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   strings.TrimSpace(token),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) < timeout {
			return ctx, func() {}
		}
	}
	return context.WithTimeout(ctx, timeout)
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set(HeaderInternalToken, c.token)
	}
	reqID := RequestIDFromContext(ctx)
	if reqID == "" {
		reqID = validateOrGenerateRequestID("")
	}
	req.Header.Set(HeaderRequestID, reqID)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("worker rpc %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBytes, _ := io.ReadAll(resp.Body)
		var errResp ErrorResponse
		if err := json.Unmarshal(errBytes, &errResp); err == nil && errResp.Error != "" {
			return fmt.Errorf("worker rpc %s returned %d: %s", path, resp.StatusCode, errResp.Error)
		}
		return fmt.Errorf("worker rpc %s returned %d: %s", path, resp.StatusCode, string(errBytes))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) RequestSync(ctx context.Context) error {
	callCtx, cancel := c.withTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.post(callCtx, "/rpc/request-sync", nil, nil)
}

func (c *Client) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	callCtx, cancel := c.withTimeout(ctx, 120*time.Second)
	defer cancel()
	var resp EnsureHistoryResponse
	err := c.post(callCtx, "/rpc/ensure-history", EnsureHistoryRequest{FromDay: fromDay, ToDay: toDay}, &resp)
	if err != nil {
		return 0, err
	}
	return resp.Count, nil
}

func (c *Client) NotifySettingsChanged(ctx context.Context) error {
	callCtx, cancel := c.withTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.post(callCtx, "/rpc/notify-settings-changed", nil, nil)
}

func (c *Client) WakeDispatcher(ctx context.Context) error {
	callCtx, cancel := c.withTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.post(callCtx, "/rpc/wake-dispatcher", nil, nil)
}

func (c *Client) VerifySession(ctx context.Context, account string, generation int64, password []byte) error {
	if len(password) == 0 {
		return errors.New("empty password")
	}
	callCtx, cancel := c.withTimeout(ctx, 20*time.Second)
	defer cancel()
	return c.post(callCtx, "/rpc/verify-session", VerifySessionRequest{
		Account:    account,
		Generation: generation,
		Password:   password,
	}, nil)
}

// Ready checks the worker /readyz endpoint from gateway
func (c *Client) Ready(ctx context.Context) error {
	callCtx, cancel := c.withTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, c.baseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set(HeaderInternalToken, c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("worker readyz check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("worker not ready (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}
