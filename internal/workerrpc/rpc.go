package workerrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	HeaderInternalToken = "X-Worker-Internal-Token"
	HeaderActorID       = "X-Actor-Id"
	HeaderRequestID     = "X-Request-Id"
	HeaderIdempotency   = "Idempotency-Key"
)

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
	NotifySettingsChanged()
	WakeDispatcher()
	VerifySession(ctx context.Context, account string, generation int64, password []byte) error
}

// Server serves private RPC requests from gateway slots
type Server struct {
	handler WorkerHandler
	token   string
	mux     *http.ServeMux

	mu          sync.Mutex
	idempotency map[string]time.Time
}

func NewServer(handler WorkerHandler, token string) *Server {
	s := &Server{
		handler:     handler,
		token:       token,
		mux:         http.NewServeMux(),
		idempotency: make(map[string]time.Time),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			reqToken := r.Header.Get(HeaderInternalToken)
			if reqToken != s.token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	s.mux.HandleFunc("/rpc/request-sync", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.handler.RequestSync(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/rpc/ensure-history", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req EnsureHistoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		count, err := s.handler.EnsureHistory(r.Context(), req.FromDay, req.ToDay)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(EnsureHistoryResponse{Count: count})
	}))

	s.mux.HandleFunc("/rpc/notify-settings-changed", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handler.NotifySettingsChanged()
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/rpc/wake-dispatcher", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handler.WakeDispatcher()
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/rpc/verify-session", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req VerifySessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.handler.VerifySession(r.Context(), req.Account, req.Generation, req.Password); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
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
		baseURL: baseURL,
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
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
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("worker rpc %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("worker rpc %s returned %d: %s", path, resp.StatusCode, string(errBytes))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) RequestSync(ctx context.Context) error {
	return c.post(ctx, "/rpc/request-sync", nil, nil)
}

func (c *Client) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	var resp EnsureHistoryResponse
	err := c.post(ctx, "/rpc/ensure-history", EnsureHistoryRequest{FromDay: fromDay, ToDay: toDay}, &resp)
	if err != nil {
		return 0, err
	}
	return resp.Count, nil
}

func (c *Client) NotifySettingsChanged() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.post(ctx, "/rpc/notify-settings-changed", nil, nil)
}

func (c *Client) WakeDispatcher() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.post(ctx, "/rpc/wake-dispatcher", nil, nil)
}

func (c *Client) VerifySession(ctx context.Context, account string, generation int64, password []byte) error {
	if len(password) == 0 {
		return errors.New("empty password")
	}
	return c.post(ctx, "/rpc/verify-session", VerifySessionRequest{
		Account:    account,
		Generation: generation,
		Password:   password,
	}, nil)
}
