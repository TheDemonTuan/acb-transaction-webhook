package ttsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	ErrUnavailable     = errors.New("tts_service_unavailable")
	ErrSynthesisFailed = errors.New("tts_synthesis_failed")
	ErrUnauthorized    = errors.New("tts_unauthorized")
	ErrOverloaded      = errors.New("tts_overloaded")
	ErrTimeout         = errors.New("tts_timeout")
)

const (
	maxSynthesisDuration = 8 * time.Second
	maxAudioBytes        = 4 << 20
)

type SynthesizeRequest struct {
	Text         string `json:"text"`
	Voice        string `json:"voice,omitempty"`
	Rate         string `json:"rate,omitempty"`
	Pitch        string `json:"pitch,omitempty"`
	Cacheable    bool   `json:"cacheable"`
	ProviderMode string `json:"provider_mode,omitempty"`
	TimeoutMS    int    `json:"timeout_ms"`
}

type SynthesizeResult struct {
	Audio    []byte
	Provider string
	Voice    string
	Fallback bool
	Cached   bool
}

type StreamResult struct {
	Reader            io.Reader
	Closer            io.Closer
	Provider          string
	Voice             string
	Fallback          bool
	Cached            bool
	FirstByteDuration time.Duration
}

func (s *StreamResult) Close() error {
	if s.Closer != nil {
		return s.Closer.Close()
	}
	return nil
}

type Client struct {
	baseURL        string
	internalToken  string
	bufferedClient *http.Client
	streamClient   *http.Client
}

func New(baseURL, internalToken string) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8081"
	}

	streamTransport := http.DefaultTransport
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := t.Clone()
		// The request context owns the entire synthesis budget, including headers.
		cloned.ResponseHeaderTimeout = 0
		streamTransport = cloned
	}

	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		internalToken: internalToken,
		bufferedClient: &http.Client{
			Transport: streamTransport,
			Timeout:   8 * time.Second,
		},
		streamClient: &http.Client{
			Transport: streamTransport,
			Timeout:   0,
		},
	}
}

func (c *Client) WithHTTPClients(buffered, stream *http.Client) *Client {
	if buffered != nil {
		c.bufferedClient = buffered
	}
	if stream != nil {
		c.streamClient = stream
	}
	return c
}

// synthesisRequest starts one total budget. Neither headers nor body reads renew it.
func (c *Client) synthesisRequest(ctx context.Context, path string, req SynthesizeRequest) (*http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, maxSynthesisDuration)
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, nil, synthesisError(ctx, err)
	}
	deadline, _ := ctx.Deadline()
	req.TimeoutMS = int(time.Until(deadline) / time.Millisecond)
	if req.TimeoutMS < 1 {
		req.TimeoutMS = 1
	}
	if req.TimeoutMS > 8000 {
		req.TimeoutMS = 8000
	}
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		cancel()
		return nil, nil, ErrSynthesisFailed
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		cancel()
		return nil, nil, ErrUnavailable
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.internalToken != "" {
		httpReq.Header.Set("X-Internal-TTS-Token", c.internalToken)
	}
	return httpReq, cancel, nil
}

// synthesisError never exposes transport URLs or upstream error text.
func synthesisError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrTimeout, context.DeadlineExceeded)
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return ErrTimeout
	}
	return ErrUnavailable
}

func synthesisStatusError(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusServiceUnavailable:
		return ErrOverloaded
	case http.StatusGatewayTimeout:
		return ErrTimeout
	case http.StatusBadGateway:
		return ErrUnavailable
	default:
		return ErrSynthesisFailed
	}
}

// synthesisBody caps streaming consumers as well as buffered reads. It probes one
// extra byte at the limit so oversized audio cannot become a successful truncation.
type synthesisBody struct {
	body      io.ReadCloser
	ctx       context.Context
	cancel    context.CancelFunc
	remaining int
	closed    bool
	readErr   error
}

func (b *synthesisBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	err := b.body.Close()
	b.cancel()
	return err
}

func (b *synthesisBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.readErr != nil {
		return 0, b.readErr
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			b.readErr = ErrSynthesisFailed
		} else if err != nil {
			b.readErr = err
			if !errors.Is(err, io.EOF) {
				b.readErr = synthesisError(b.ctx, err)
			}
		} else {
			return 0, nil
		}
		_ = b.Close()
		return 0, b.readErr
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= n
	if err != nil {
		b.readErr = err
		if !errors.Is(err, io.EOF) {
			b.readErr = synthesisError(b.ctx, err)
		}
		_ = b.Close()
		err = b.readErr
	}
	return n, err
}

func (c *Client) SynthesizeStream(ctx context.Context, req SynthesizeRequest) (*StreamResult, error) {
	httpReq, cancel, err := c.synthesisRequest(ctx, "/synthesize/stream", req)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := c.streamClient.Do(httpReq)
	if err != nil {
		err = synthesisError(httpReq.Context(), err)
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		return nil, synthesisStatusError(resp.StatusCode)
	}
	body := &synthesisBody{body: resp.Body, ctx: httpReq.Context(), cancel: cancel, remaining: maxAudioBytes}
	buf := make([]byte, 4096)
	n, err := body.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = body.Close()
		return nil, err
	}
	provider := resp.Header.Get("X-TTS-Provider")
	return &StreamResult{
		Reader:            io.MultiReader(bytes.NewReader(buf[:n]), body),
		Closer:            body,
		Provider:          provider,
		Voice:             resp.Header.Get("X-TTS-Voice"),
		Fallback:          strings.EqualFold(resp.Header.Get("X-TTS-Fallback"), "true"),
		Cached:            strings.EqualFold(resp.Header.Get("X-TTS-Cached"), "true"),
		FirstByteDuration: time.Since(start),
	}, nil
}

func (c *Client) Synthesize(ctx context.Context, req SynthesizeRequest) (*SynthesizeResult, error) {
	httpReq, cancel, err := c.synthesisRequest(ctx, "/synthesize", req)
	if err != nil {
		return nil, err
	}
	defer cancel()
	resp, err := c.bufferedClient.Do(httpReq)
	if err != nil {
		return nil, synthesisError(httpReq.Context(), err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, synthesisStatusError(resp.StatusCode)
	}
	body := &synthesisBody{body: resp.Body, ctx: httpReq.Context(), cancel: cancel, remaining: maxAudioBytes}
	defer body.Close()
	audioData, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	provider := resp.Header.Get("X-TTS-Provider")
	return &SynthesizeResult{
		Audio:    audioData,
		Provider: provider,
		Voice:    resp.Header.Get("X-TTS-Voice"),
		Fallback: strings.EqualFold(resp.Header.Get("X-TTS-Fallback"), "true"),
		Cached:   strings.EqualFold(resp.Header.Get("X-TTS-Cached"), "true"),
	}, nil
}
