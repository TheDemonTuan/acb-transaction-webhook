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
)

type SynthesizeRequest struct {
	Text          string `json:"text"`
	Voice         string `json:"voice,omitempty"`
	Rate          string `json:"rate,omitempty"`
	Pitch         string `json:"pitch,omitempty"`
	Cacheable     bool   `json:"cacheable"`
	AllowFallback *bool  `json:"allow_fallback,omitempty"`
	ProviderMode  string `json:"provider_mode,omitempty"`
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
	baseURL       string
	internalToken string
	httpClient    *http.Client
}

func New(baseURL, internalToken string) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8081"
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		internalToken: internalToken,
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
	}
}

func (c *Client) SynthesizeStream(ctx context.Context, req SynthesizeRequest) (*StreamResult, error) {
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/synthesize/stream"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if c.internalToken != "" {
		httpReq.Header.Set("X-Internal-TTS-Token", c.internalToken)
	}

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, ErrUnauthorized
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return c.synthesizeStreamBufferedFallback(ctx, req)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%w (status %d): %s", ErrSynthesisFailed, resp.StatusCode, string(respBody))
	}

	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("read initial chunk: %w", err)
	}
	firstByteDuration := time.Since(start)

	provider := resp.Header.Get("X-TTS-Provider")
	if provider == "" {
		provider = "edge"
	}
	voice := resp.Header.Get("X-TTS-Voice")
	fallback := strings.EqualFold(resp.Header.Get("X-TTS-Fallback"), "true")
	cached := strings.EqualFold(resp.Header.Get("X-TTS-Cached"), "true")

	var reader io.Reader
	if n > 0 {
		reader = io.MultiReader(bytes.NewReader(buf[:n]), resp.Body)
	} else {
		reader = resp.Body
	}

	return &StreamResult{
		Reader:            reader,
		Closer:            resp.Body,
		Provider:          provider,
		Voice:             voice,
		Fallback:          fallback,
		Cached:            cached,
		FirstByteDuration: firstByteDuration,
	}, nil
}

func (c *Client) synthesizeStreamBufferedFallback(ctx context.Context, req SynthesizeRequest) (*StreamResult, error) {
	start := time.Now()
	res, err := c.synthesizeBuffered(ctx, req)
	if err != nil {
		return nil, err
	}
	firstByteDuration := time.Since(start)
	return &StreamResult{
		Reader:            bytes.NewReader(res.Audio),
		Closer:            io.NopCloser(bytes.NewReader(nil)),
		Provider:          res.Provider,
		Voice:             res.Voice,
		Fallback:          res.Fallback,
		Cached:            res.Cached,
		FirstByteDuration: firstByteDuration,
	}, nil
}

func (c *Client) Synthesize(ctx context.Context, req SynthesizeRequest) (*SynthesizeResult, error) {
	stream, err := c.SynthesizeStream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	audioData, err := io.ReadAll(io.LimitReader(stream.Reader, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}

	return &SynthesizeResult{
		Audio:    audioData,
		Provider: stream.Provider,
		Voice:    stream.Voice,
		Fallback: stream.Fallback,
		Cached:   stream.Cached,
	}, nil
}

func (c *Client) synthesizeBuffered(ctx context.Context, req SynthesizeRequest) (*SynthesizeResult, error) {
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/synthesize"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if c.internalToken != "" {
		httpReq.Header.Set("X-Internal-TTS-Token", c.internalToken)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%w (status %d): %s", ErrSynthesisFailed, resp.StatusCode, string(respBody))
	}

	audioData, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}

	provider := resp.Header.Get("X-TTS-Provider")
	if provider == "" {
		provider = "edge"
	}
	voice := resp.Header.Get("X-TTS-Voice")
	fallback := strings.EqualFold(resp.Header.Get("X-TTS-Fallback"), "true")
	cached := strings.EqualFold(resp.Header.Get("X-TTS-Cached"), "true")

	return &SynthesizeResult{
		Audio:    audioData,
		Provider: provider,
		Voice:    voice,
		Fallback: fallback,
		Cached:   cached,
	}, nil
}
