package realtimestream

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
)

const (
	DefaultPath           = "/internal/v1/events"
	HeaderInternalToken   = "X-Worker-Internal-Token"
	DefaultWriteTimeout   = 5 * time.Second
	DefaultHeartbeat      = 15 * time.Second
	DefaultMaxSubscribers = 32
)

type ServerConfig struct {
	Hub            *eventhub.Hub
	Token          string
	Path           string
	MaxSubscribers int
	Heartbeat      time.Duration
	WriteTimeout   time.Duration
}

type Server struct {
	hub            *eventhub.Hub
	token          string
	path           string
	maxSubscribers int
	heartbeat      time.Duration
	writeTimeout   time.Duration
	currentSubs    atomic.Int64
}

func NewServer(cfg ServerConfig) *Server {
	path := cfg.Path
	if path == "" {
		path = DefaultPath
	}
	maxSubs := cfg.MaxSubscribers
	if maxSubs <= 0 {
		maxSubs = DefaultMaxSubscribers
	}
	hb := cfg.Heartbeat
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	wt := cfg.WriteTimeout
	if wt <= 0 {
		wt = DefaultWriteTimeout
	}

	return &Server{
		hub:            cfg.Hub,
		token:          strings.TrimSpace(cfg.Token),
		path:           path,
		maxSubscribers: maxSubs,
		heartbeat:      hb,
		writeTimeout:   wt,
	}
}

// ActiveSubscribers returns current count of active SSE client connections.
func (s *Server) ActiveSubscribers() int {
	return int(s.currentSubs.Load())
}

// Path returns the configured endpoint path.
func (s *Server) Path() string {
	return s.path
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.path != "" && r.URL.Path != s.path && r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reqToken := strings.TrimSpace(r.Header.Get(HeaderInternalToken))
	if reqToken == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			reqToken = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}

	if s.token == "" || subtle.ConstantTimeCompare([]byte(reqToken), []byte(s.token)) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	if s.hub == nil {
		http.Error(w, "event stream unavailable", http.StatusServiceUnavailable)
		return
	}

	if s.maxSubscribers > 0 {
		for {
			cur := s.currentSubs.Load()
			if cur >= int64(s.maxSubscribers) {
				http.Error(w, `{"error":"too many subscribers"}`, http.StatusTooManyRequests)
				return
			}
			if s.currentSubs.CompareAndSwap(cur, cur+1) {
				break
			}
		}
		defer s.currentSubs.Add(-1)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	_, ch, cancel := s.hub.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
			_ = rc.SetWriteDeadline(time.Time{})
		case ev, ok := <-ch:
			if !ok {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
			_ = rc.SetWriteDeadline(time.Time{})
		}
	}
}

type eventEnvelope struct {
	Seq         int64           `json:"seq"`
	Epoch       string          `json:"epoch"`
	EventType   string          `json:"eventType"`
	AggregateID string          `json:"aggregateId,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   string          `json:"createdAt,omitempty"`
}

func writeSSEEvent(w io.Writer, ev eventhub.Event) error {
	raw := ev.Payload
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("null")
	} else if !json.Valid(raw) {
		b, err := json.Marshal(string(raw))
		if err != nil {
			return err
		}
		raw = json.RawMessage(b)
	}

	env := eventEnvelope{
		Seq:         ev.Seq,
		Epoch:       ev.Epoch,
		EventType:   ev.EventType,
		AggregateID: ev.AggregateID,
		Payload:     raw,
		CreatedAt:   ev.CreatedAt,
	}

	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	if ev.Epoch != "" {
		if _, err := fmt.Fprintf(w, "id: %s:%d\n", ev.Epoch, ev.Seq); err != nil {
			return err
		}
	} else if ev.Seq != 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", ev.Seq); err != nil {
			return err
		}
	}

	if ev.EventType != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", ev.EventType); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}

	return nil
}
