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

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerstate"
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

// CreateHistoryJobRequest payload for enqueuing a durable historical backfill job
type CreateHistoryJobRequest struct {
	FromDay string `json:"fromDay"`
	ToDay   string `json:"toDay"`
}

// VerifySessionRequest payload for verifying credentials with bank
type VerifySessionRequest struct {
	Account    string `json:"account"`
	Generation int64  `json:"generation"`
	Password   []byte `json:"password"`
}

type TestNotificationRequest struct {
	ChannelID string `json:"channelId"`
}

type TestNotificationResponse struct {
	Success           bool   `json:"success"`
	Status            string `json:"status"`
	LatencyMs         int64  `json:"latencyMs"`
	Message           string `json:"message,omitempty"`
	ProviderErrorCode string `json:"code,omitempty"`
	SanitizedError    string `json:"error,omitempty"`
}

type QuiesceResponse struct {
	Status     string `json:"status"`
	Quiesced   bool   `json:"quiesced"`
	Generation int64  `json:"generation"`
	Checkpoint string `json:"checkpoint,omitempty"`
	CoverageTo string `json:"coverageTo,omitempty"`
	ScanID     string `json:"scanId,omitempty"`
	WorkerID   string `json:"workerId,omitempty"`
}

type ResumeResponse struct {
	Status  string `json:"status"`
	Resumed bool   `json:"resumed"`
}

type NotificationProviderMetadata struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Configured  bool   `json:"configured"`
	PublicURL   string `json:"publicUrl,omitempty"`
	Status      string `json:"status,omitempty"`
}

type NotificationProvidersResponse struct {
	Providers []NotificationProviderMetadata `json:"providers"`
}

type NotificationProviderReader interface {
	NotificationProviderMetadata(ctx context.Context) (NotificationProvidersResponse, error)
}

// Handler interface implemented by worker
type WorkerHandler interface {
	RequestSync(ctx context.Context) error
	CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error)
	CancelHistoryJob(ctx context.Context, jobID string) error
	NotifySettingsChanged(ctx context.Context) error
	WakeDispatcher(ctx context.Context) error
	VerifySession(ctx context.Context, account string, generation int64, password []byte) error
	TestNotificationChannel(ctx context.Context, channelID string) (TestNotificationResponse, error)
	Quiesce(ctx context.Context) (QuiesceResponse, error)
	Resume(ctx context.Context) error
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

	mu            sync.Mutex
	readyChecker  func(ctx context.Context) error
	drainHandler  func(ctx context.Context) error
	stateProvider func() workerstate.State

	runtimeRole       string
	releaseCommit     string
	slot              string
	schemaVersion     string
	heartbeatProvider func() time.Time
	staleThreshold    time.Duration
	providerReader    NotificationProviderReader
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
		handler:        handler,
		token:          trimmedToken,
		mux:            http.NewServeMux(),
		maxBodyBytes:   1 << 20, // 1MB
		serverTimeout:  30 * time.Second,
		sem:            make(chan struct{}, 32),
		runtimeRole:    "worker",
		staleThreshold: 60 * time.Second,
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

func (s *Server) SetRuntimeInfo(role, release, slot, schema string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if role != "" {
		s.runtimeRole = role
	}
	s.releaseCommit = release
	s.slot = slot
	s.schemaVersion = schema
}

func (s *Server) SetHeartbeatProvider(provider func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatProvider = provider
}

func (s *Server) SetStaleThreshold(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > 0 {
		s.staleThreshold = d
	}
}

func (s *Server) SetReadyChecker(checker func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyChecker = checker
}

func (s *Server) SetDrainHandler(drainer func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drainHandler = drainer
}

func (s *Server) SetStateProvider(provider func() workerstate.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateProvider = provider
}

func (s *Server) SetProviderReader(reader NotificationProviderReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providerReader = reader
}

func (s *Server) checkWorkAllowed() error {
	s.mu.Lock()
	sp := s.stateProvider
	s.mu.Unlock()
	if sp != nil {
		state := sp()
		if state != workerstate.StateReady {
			return fmt.Errorf("worker is in %s state", state)
		}
	}
	return nil
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

func (s *Server) setPlatformHeaders(w http.ResponseWriter) {
	s.mu.Lock()
	role := s.runtimeRole
	rel := s.releaseCommit
	slot := s.slot
	s.mu.Unlock()
	if role != "" {
		w.Header().Set("X-Runtime-Role", role)
	}
	if rel != "" {
		w.Header().Set("X-Release-Commit", rel)
	}
	if slot != "" {
		w.Header().Set("X-Platform-Slot", slot)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		reqID := validateOrGenerateRequestID(r.Header.Get(HeaderRequestID))
		w.Header().Set(HeaderRequestID, reqID)
		s.setPlatformHeaders(w)

		s.mu.Lock()
		role := s.runtimeRole
		rel := s.releaseCommit
		s.mu.Unlock()

		// Role check
		reqRole := r.URL.Query().Get("role")
		if reqRole == "" {
			reqRole = r.Header.Get("X-Expected-Role")
		}
		if reqRole != "" && !strings.EqualFold(reqRole, role) && !strings.EqualFold(role, "all-in-one") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":    "error",
				"error":     fmt.Sprintf("role mismatch: expected %s, got %s", reqRole, role),
				"role":      role,
				"requestId": reqID,
			})
			return
		}

		// Release check
		reqRel := r.URL.Query().Get("release")
		if reqRel == "" {
			reqRel = r.Header.Get("X-Expected-Release")
		}
		if reqRel != "" && rel != "" && rel != reqRel {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":    "error",
				"error":     fmt.Sprintf("release mismatch: expected %s, got %s", reqRel, rel),
				"release":   rel,
				"requestId": reqID,
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "ok",
			"role":      role,
			"release":   rel,
			"requestId": reqID,
		})
	})

	s.mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		reqID := validateOrGenerateRequestID(r.Header.Get(HeaderRequestID))
		w.Header().Set(HeaderRequestID, reqID)
		s.setPlatformHeaders(w)

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}

		s.mu.Lock()
		role := s.runtimeRole
		rel := s.releaseCommit
		schema := s.schemaVersion
		hbProvider := s.heartbeatProvider
		staleThresh := s.staleThreshold
		checker := s.readyChecker
		sp := s.stateProvider
		s.mu.Unlock()

		// 1. Role check
		reqRole := r.URL.Query().Get("role")
		if reqRole == "" {
			reqRole = r.Header.Get("X-Expected-Role")
		}
		if reqRole != "" && !strings.EqualFold(reqRole, role) && !strings.EqualFold(role, "all-in-one") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":    "not_ready",
				"error":     fmt.Sprintf("role mismatch: expected %s, got %s", reqRole, role),
				"role":      role,
				"requestId": reqID,
			})
			return
		}

		// 2. Release check
		reqRel := r.URL.Query().Get("release")
		if reqRel == "" {
			reqRel = r.Header.Get("X-Expected-Release")
		}
		if reqRel != "" && rel != "" && rel != reqRel {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":    "not_ready",
				"error":     fmt.Sprintf("release mismatch: expected %s, got %s", reqRel, rel),
				"release":   rel,
				"requestId": reqID,
			})
			return
		}

		// 3. Schema check
		reqSchema := r.URL.Query().Get("schema")
		if reqSchema == "" {
			reqSchema = r.Header.Get("X-Expected-Schema")
		}
		if reqSchema != "" && schema != "" && schema != reqSchema {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":    "not_ready",
				"error":     fmt.Sprintf("schema mismatch: expected %s, got %s", reqSchema, schema),
				"schema":    schema,
				"requestId": reqID,
			})
			return
		}

		// 4. Stale check
		if hbProvider != nil {
			lastHb := hbProvider()
			if staleThresh <= 0 {
				staleThresh = 60 * time.Second
			}
			if !lastHb.IsZero() && time.Since(lastHb) > staleThresh {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"status":    "not_ready",
					"stale":     true,
					"error":     fmt.Sprintf("worker is stale: last heartbeat was %.0fs ago", time.Since(lastHb).Seconds()),
					"requestId": reqID,
				})
				return
			}
		}

		// 5. Ready checker
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

		var stateStr string
		if sp != nil {
			stateStr = string(sp())
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":           "ready",
			"role":             role,
			"release":          rel,
			"schemaVersion":    schema,
			"workerRpcVersion": "v2",
			"state":            stateStr,
			"singletonLock":    true,
			"stale":            false,
			"requestId":        reqID,
		})
	})

	s.mux.HandleFunc("/internal/deployz", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := validateOrGenerateRequestID(r.Header.Get(HeaderRequestID))
		w.Header().Set(HeaderRequestID, reqID)
		s.setPlatformHeaders(w)

		s.mu.Lock()
		role := s.runtimeRole
		rel := s.releaseCommit
		slot := s.slot
		schema := s.schemaVersion
		hbProvider := s.heartbeatProvider
		staleThresh := s.staleThreshold
		checker := s.readyChecker
		sp := s.stateProvider
		s.mu.Unlock()

		status := "ready"
		resp := map[string]any{
			"role":             role,
			"release":          rel,
			"slot":             slot,
			"schemaVersion":    schema,
			"workerRpcVersion": "v2",
			"singletonLock":    true,
			"requestId":        reqID,
		}

		// Role check
		reqRole := r.URL.Query().Get("role")
		if reqRole == "" {
			reqRole = r.Header.Get("X-Expected-Role")
		}
		if reqRole != "" && !strings.EqualFold(reqRole, role) && !strings.EqualFold(role, "all-in-one") {
			resp["roleError"] = fmt.Sprintf("expected %s, got %s", reqRole, role)
			status = "not_ready"
		}

		// Release check
		reqRel := r.URL.Query().Get("release")
		if reqRel == "" {
			reqRel = r.Header.Get("X-Expected-Release")
		}
		if reqRel != "" && rel != "" && rel != reqRel {
			resp["releaseError"] = fmt.Sprintf("expected %s, got %s", reqRel, rel)
			status = "not_ready"
		}

		// Schema check
		reqSchema := r.URL.Query().Get("schema")
		if reqSchema == "" {
			reqSchema = r.Header.Get("X-Expected-Schema")
		}
		if reqSchema != "" && schema != "" && schema != reqSchema {
			resp["schemaError"] = fmt.Sprintf("expected %s, got %s", reqSchema, schema)
			status = "not_ready"
		}

		// Stale check
		var isStale bool
		if hbProvider != nil {
			lastHb := hbProvider()
			if staleThresh <= 0 {
				staleThresh = 60 * time.Second
			}
			if !lastHb.IsZero() && time.Since(lastHb) > staleThresh {
				isStale = true
				status = "not_ready"
				resp["staleError"] = fmt.Sprintf("last heartbeat was %.0fs ago", time.Since(lastHb).Seconds())
			}
		}
		resp["stale"] = isStale

		// Readiness checker
		if checker != nil {
			if err := checker(r.Context()); err != nil {
				resp["checkerError"] = err.Error()
				status = "not_ready"
			}
		}

		if sp != nil {
			resp["state"] = string(sp())
		}

		resp["status"] = status
		code := http.StatusOK
		if status == "not_ready" {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, resp)
	}))

	s.mux.HandleFunc("/rpc/drain", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		s.mu.Lock()
		drainer := s.drainHandler
		s.mu.Unlock()
		if drainer != nil {
			if err := drainer(r.Context()); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error(), reqID)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "draining", "requestId": reqID})
	}))

	s.mux.HandleFunc("/rpc/quiesce", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		resp, err := s.handler.Quiesce(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	s.mux.HandleFunc("/rpc/resume", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		if err := s.handler.Resume(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, ResumeResponse{Status: "ok", Resumed: true})
	}))

	s.mux.HandleFunc("/rpc/request-sync", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		if err := s.checkWorkAllowed(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":     err.Error(),
				"code":      "WORKER_DRAINING",
				"requestId": reqID,
			})
			return
		}
		if err := s.handler.RequestSync(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "requestId": reqID})
	}))

	s.mux.HandleFunc("/rpc/history-jobs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		if err := s.checkWorkAllowed(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":     err.Error(),
				"code":      "WORKER_DRAINING",
				"requestId": reqID,
			})
			return
		}
		var req CreateHistoryJobRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad request", reqID)
			return
		}
		fromT, err := time.Parse("2006-01-02", req.FromDay)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid fromDay: "+err.Error(), reqID)
			return
		}
		toT, err := time.Parse("2006-01-02", req.ToDay)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid toDay: "+err.Error(), reqID)
			return
		}
		if fromT.After(toT) {
			writeError(w, http.StatusBadRequest, "fromDay must not be after toDay", reqID)
			return
		}
		if toT.Sub(fromT) > 31*24*time.Hour {
			writeError(w, http.StatusBadRequest, "range too large (max 31 days)", reqID)
			return
		}
		job, err := s.handler.CreateHistoryJob(r.Context(), req.FromDay, req.ToDay)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, job)
	}))

	cancelHistoryJobHandler := s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		jobID := r.PathValue("jobID")
		if jobID == "" {
			path := strings.TrimPrefix(r.URL.Path, "/rpc/history-jobs/")
			jobID = strings.TrimSuffix(path, "/cancel")
		}
		if jobID == "" {
			writeError(w, http.StatusBadRequest, "job ID is required", reqID)
			return
		}
		if err := s.handler.CancelHistoryJob(r.Context(), jobID); err != nil {
			if errors.Is(err, storage.ErrJobNotFound) {
				writeError(w, http.StatusNotFound, err.Error(), reqID)
				return
			}
			if errors.Is(err, storage.ErrJobTerminal) {
				writeError(w, http.StatusConflict, err.Error(), reqID)
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "requestId": reqID})
	})
	s.mux.HandleFunc("/rpc/history-jobs/{jobID}/cancel", cancelHistoryJobHandler)
	s.mux.HandleFunc("/rpc/history-jobs/cancel", cancelHistoryJobHandler)

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
		if err := s.checkWorkAllowed(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":     err.Error(),
				"code":      "WORKER_DRAINING",
				"requestId": reqID,
			})
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

	s.mux.HandleFunc("/rpc/notification-channels/test", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		if err := s.checkWorkAllowed(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":     err.Error(),
				"code":      "WORKER_DRAINING",
				"requestId": reqID,
			})
			return
		}
		var req TestNotificationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad request: "+err.Error(), reqID)
			return
		}
		if strings.TrimSpace(req.ChannelID) == "" {
			writeError(w, http.StatusBadRequest, "channelId is required", reqID)
			return
		}
		resp, err := s.handler.TestNotificationChannel(r.Context(), req.ChannelID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}))

	s.mux.HandleFunc("/rpc/notification-providers", s.auth(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(HeaderRequestID)
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", reqID)
			return
		}
		s.mu.Lock()
		pr := s.providerReader
		s.mu.Unlock()
		if pr == nil {
			if reader, ok := s.handler.(NotificationProviderReader); ok {
				pr = reader
			}
		}
		if pr == nil {
			writeError(w, http.StatusNotImplemented, "provider metadata reader not implemented", reqID)
			return
		}
		resp, err := pr.NotificationProviderMetadata(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), reqID)
			return
		}
		writeJSON(w, http.StatusOK, resp)
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
		client:  &http.Client{Timeout: 35 * time.Second},
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

func (c *Client) Drain(ctx context.Context) error {
	callCtx, cancel := c.withTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.post(callCtx, "/rpc/drain", nil, nil)
}

func (c *Client) Quiesce(ctx context.Context) (QuiesceResponse, error) {
	callCtx, cancel := c.withTimeout(ctx, 25*time.Second)
	defer cancel()
	var resp QuiesceResponse
	err := c.post(callCtx, "/rpc/quiesce", nil, &resp)
	return resp, err
}

func (c *Client) Resume(ctx context.Context) error {
	callCtx, cancel := c.withTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.post(callCtx, "/rpc/resume", nil, nil)
}

func (c *Client) CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error) {
	callCtx, cancel := c.withTimeout(ctx, 5*time.Second)
	defer cancel()
	var job storage.HistorySyncJob
	err := c.post(callCtx, "/rpc/history-jobs", CreateHistoryJobRequest{FromDay: fromDay, ToDay: toDay}, &job)
	if err != nil {
		return storage.HistorySyncJob{}, err
	}
	return job, nil
}

func (c *Client) CancelHistoryJob(ctx context.Context, jobID string) error {
	callCtx, cancel := c.withTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.post(callCtx, fmt.Sprintf("/rpc/history-jobs/%s/cancel", jobID), nil, nil)
}

// EnsureHistory is a transition compatibility helper for callers requiring the HistoryEnsurer interface before PR07.
func (c *Client) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	job, err := c.CreateHistoryJob(ctx, fromDay, toDay)
	if err != nil {
		return 0, err
	}
	return job.RowsSeen, nil
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

func (c *Client) TestNotificationChannel(ctx context.Context, channelID string) (TestNotificationResponse, error) {
	callCtx, cancel := c.withTimeout(ctx, 15*time.Second)
	defer cancel()
	var resp TestNotificationResponse
	err := c.post(callCtx, "/rpc/notification-channels/test", TestNotificationRequest{ChannelID: channelID}, &resp)
	return resp, err
}

func (c *Client) NotificationProviderMetadata(ctx context.Context) (NotificationProvidersResponse, error) {
	callCtx, cancel := c.withTimeout(ctx, 5*time.Second)
	defer cancel()
	var resp NotificationProvidersResponse
	err := c.post(callCtx, "/rpc/notification-providers", nil, &resp)
	return resp, err
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
