package realtimestream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
)

var (
	ErrUnauthorized  = errors.New("realtimestream: unauthorized")
	ErrFrameTooLarge = errors.New("realtimestream: frame exceeds max size")
	ErrStreamIdle    = errors.New("realtimestream: stream idle timeout")
)

type HandlerError struct {
	Err error
}

func (e *HandlerError) Error() string {
	return fmt.Sprintf("realtimestream: handler error: %v", e.Err)
}

func (e *HandlerError) Unwrap() error {
	return e.Err
}

type ClientConfig struct {
	BaseURL        string
	Token          string
	HTTPClient     *http.Client
	MaxFrameBytes  int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	IdleTimeout    time.Duration
	OnConnect      func()
	OnConnectError func(error)
	OnDisconnect   func(error)
	OnReconnect    func()
}

type Client struct {
	url            string
	token          string
	httpClient     *http.Client
	maxFrameBytes  int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	idleTimeout    time.Duration
	onConnect      func()
	onConnectError func(error)
	onDisconnect   func(error)
	onReconnect    func()

	mu          sync.Mutex
	lastEventID string
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("realtimestream: missing BaseURL")
	}

	rawURL := cfg.BaseURL
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "http://" + rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("realtimestream: invalid url: %w", err)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = DefaultPath
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 0,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	} else if httpClient.CheckRedirect == nil {
		httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	maxFrame := cfg.MaxFrameBytes
	if maxFrame <= 0 {
		maxFrame = 1024 * 1024
	}
	initBackoff := cfg.InitialBackoff
	if initBackoff <= 0 {
		initBackoff = 500 * time.Millisecond
	}
	maxBackoff := cfg.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Second
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 45 * time.Second
	}

	return &Client{
		url:            parsed.String(),
		token:          strings.TrimSpace(cfg.Token),
		httpClient:     httpClient,
		maxFrameBytes:  maxFrame,
		initialBackoff: initBackoff,
		maxBackoff:     maxBackoff,
		idleTimeout:    idleTimeout,
		onConnect:      cfg.OnConnect,
		onConnectError: cfg.OnConnectError,
		onDisconnect:   cfg.OnDisconnect,
		onReconnect:    cfg.OnReconnect,
	}, nil
}

func (c *Client) LastEventID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEventID
}

func (c *Client) SetLastEventID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastEventID = id
}

// Connect opens an SSE connection and returns the HTTP response.
func (c *Client) Connect(ctx context.Context) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("realtimestream: build request: %w", err)
	}

	if c.token != "" {
		req.Header.Set(HeaderInternalToken, c.token)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	c.mu.Lock()
	lastID := c.lastEventID
	c.mu.Unlock()
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, ErrUnauthorized
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("realtimestream: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return resp, nil
}

// Consume streams events from a single connection until ctx is canceled, an error occurs, or handler returns error.
func (c *Client) Consume(ctx context.Context, handle func(eventhub.Event) error) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := c.Connect(streamCtx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if c.onConnect != nil {
		c.onConnect()
	}

	idle := time.AfterFunc(c.idleTimeout, func() {
		cancel()
		_ = resp.Body.Close()
	})
	defer idle.Stop()
	err = c.parseStream(resp.Body, handle, func() { idle.Reset(c.idleTimeout) })
	if streamCtx.Err() != nil && ctx.Err() == nil {
		err = ErrStreamIdle
	}
	if ctx.Err() == nil && c.onDisconnect != nil {
		c.onDisconnect(err)
	}
	return err
}

// Run persistently consumes events, automatically reconnecting on transient disconnects.
func (c *Client) Run(ctx context.Context, handle func(eventhub.Event) error) error {
	backoff := c.initialBackoff
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 && c.onReconnect != nil {
			c.onReconnect()
		}

		start := time.Now()
		err := c.Consume(ctx, handle)
		attempt++
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if c.onConnectError != nil {
				c.onConnectError(err)
			}

			var hErr *HandlerError
			if errors.As(err, &hErr) {
				return hErr.Err
			}

			if errors.Is(err, ErrUnauthorized) {
				return err
			}
		}

		if time.Since(start) > 5*time.Second {
			backoff = c.initialBackoff
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		backoff = min(backoff*2, c.maxBackoff)
	}
}

func (c *Client) parseStream(r io.Reader, handle func(eventhub.Event) error, onActivity func()) error {
	br := bufio.NewReaderSize(r, 64*1024)

	var curID string
	var curEvent string
	var curData bytes.Buffer
	var frameBytes int

	for {
		line, err := readBoundedLine(br, c.maxFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if curData.Len() > 0 || curEvent != "" || curID != "" {
					ev, parseErr := parseSSEEvent(curID, curEvent, curData.Bytes())
					if parseErr != nil {
						return parseErr
					}
					if handle != nil {
						if handleErr := handle(ev); handleErr != nil {
							return &HandlerError{Err: handleErr}
						}
					}
					if curID != "" {
						c.SetLastEventID(curID)
					}
				}
				return io.EOF
			}
			return err
		}

		if onActivity != nil {
			onActivity()
		}

		// Comment or heartbeat line starting with ':'
		if len(line) > 0 && line[0] == ':' {
			continue
		}

		// Empty line marks event frame boundary
		if len(line) == 0 {
			if curData.Len() > 0 || curEvent != "" || curID != "" {
				ev, err := parseSSEEvent(curID, curEvent, curData.Bytes())
				if err != nil {
					return err
				}
				if handle != nil {
					if err := handle(ev); err != nil {
						return &HandlerError{Err: err}
					}
				}
				if curID != "" {
					c.SetLastEventID(curID)
				}
				curID = ""
				curEvent = ""
				curData.Reset()
				frameBytes = 0
			}
			continue
		}

		frameBytes += len(line)
		if frameBytes > c.maxFrameBytes {
			return ErrFrameTooLarge
		}

		colonIdx := bytes.IndexByte(line, ':')
		var field, value []byte
		if colonIdx >= 0 {
			field = line[:colonIdx]
			value = line[colonIdx+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		} else {
			field = line
			value = nil
		}

		switch string(field) {
		case "event":
			curEvent = string(value)
		case "id":
			curID = string(value)
		case "data":
			if curData.Len() > 0 {
				curData.WriteByte('\n')
			}
			curData.Write(value)
		}
	}
}

func readBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if len(chunk) > 0 {
			if len(line)+len(chunk) > maxBytes {
				return nil, ErrFrameTooLarge
			}
			line = append(line, chunk...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		if !isPrefix {
			break
		}
	}
	return line, nil
}

func parseSSEEvent(id, event string, data []byte) (eventhub.Event, error) {
	var ev eventhub.Event

	var env struct {
		Seq         int64           `json:"seq"`
		Epoch       string          `json:"epoch"`
		EventType   string          `json:"eventType"`
		AggregateID string          `json:"aggregateId"`
		Payload     json.RawMessage `json:"payload"`
		CreatedAt   string          `json:"createdAt"`
		CommittedAt string          `json:"committedAt"`
	}

	if err := json.Unmarshal(data, &env); err == nil && env.Payload != nil {
		ev.Seq = env.Seq
		ev.Epoch = env.Epoch
		ev.EventType = env.EventType
		ev.AggregateID = env.AggregateID
		ev.CreatedAt = env.CreatedAt
		ev.CommittedAt = env.CommittedAt
		if string(env.Payload) != "null" {
			ev.Payload = []byte(env.Payload)
		}
	} else {
		ev.Payload = data
	}

	if id != "" {
		parts := strings.SplitN(id, ":", 2)
		if len(parts) == 2 {
			ev.Epoch = parts[0]
			if s, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				ev.Seq = s
			}
		} else if s, err := strconv.ParseInt(id, 10, 64); err == nil {
			ev.Seq = s
		}
	}

	if event != "" {
		ev.EventType = event
	}

	return ev, nil
}
