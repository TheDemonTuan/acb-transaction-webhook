package payments

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
)

const (
	providerBaseURL                = "https://api-merchant.payos.vn"
	providerHTTPTimeout            = 10 * time.Second
	providerRequestTimeout         = 15 * time.Second
	maxProviderResponseBytes       = 1 << 20
	maxSafeInteger           int64 = 9007199254740991
)

// PayOSOption supplies fixture dependencies; production callers omit options.
// There is deliberately no environment setting for an alternate provider URL.
type PayOSOption func(*payOSDependencies)

type payOSDependencies struct {
	baseURL    string
	httpClient *http.Client
}

func WithHTTPClient(client *http.Client) PayOSOption {
	return func(opts *payOSDependencies) { opts.httpClient = client }
}

func WithBaseURL(baseURL string) PayOSOption {
	return func(opts *payOSDependencies) { opts.baseURL = baseURL }
}

type PayOSAdapter struct{ sdk *payos.PayOS }

var _ Provider = (*PayOSAdapter)(nil)

func NewPayOS(clientID, apiKey, checksumKey string, options ...PayOSOption) (*PayOSAdapter, error) {
	// Check explicitly so SDK environment fallbacks cannot supply another channel.
	if clientID == "" || apiKey == "" || checksumKey == "" {
		return nil, errors.New("payment provider credentials are incomplete")
	}
	dependencies := &payOSDependencies{baseURL: providerBaseURL}
	for _, option := range options {
		option(dependencies)
	}
	if dependencies.baseURL == "" {
		dependencies.baseURL = providerBaseURL
	}
	client := &http.Client{}
	if dependencies.httpClient != nil {
		*client = *dependencies.httpClient
	}
	client.Timeout = providerHTTPTimeout
	// Do not forward channel credentials to a redirect target, even on the same host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	sdk, err := payos.NewPayOS(&payos.PayOSOptions{
		ClientId: clientID, ApiKey: apiKey, ChecksumKey: checksumKey,
		BaseURL: dependencies.baseURL, HTTPClient: client,
		Timeout: providerHTTPTimeout, MaxRetries: 2,
		DebugLogger: nil, Middlewares: []payos.Middleware{captureResponse},
	})
	if err != nil {
		return nil, errors.New("payment provider initialization failed")
	}
	return &PayOSAdapter{sdk: sdk}, nil
}

type captureKey struct{}
type responseCapture struct {
	status           int
	code             string
	retryAfter       time.Duration
	transportFailure bool
}

func providerContext(ctx context.Context) (context.Context, context.CancelFunc, *responseCapture) {
	ctx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	capture := &responseCapture{}
	return context.WithValue(ctx, captureKey{}, capture), cancel, capture
}

func (c *responseCapture) failure() error {
	// Only an actual 404 is evidence of absence. Unknown 200 envelope codes,
	// transport/integrity failures, throttling and server failures remain uncertain.
	definitive := !c.transportFailure && c.status >= 400 && c.status < 500 &&
		c.status != http.StatusRequestTimeout && c.status != http.StatusTooManyRequests
	return &ProviderError{HTTPStatus: c.status, Code: c.code, RetryAfter: c.retryAfter, Indeterminate: !definitive}
}

func (p *PayOSAdapter) Create(ctx context.Context, req payos.CreatePaymentLinkRequest) (*payos.CreatePaymentLinkResponse, error) {
	ctx, cancel, capture := providerContext(ctx)
	defer cancel()
	result, err := p.sdk.PaymentRequests.Create(ctx, req)
	if err != nil {
		return nil, capture.failure()
	}
	return result, nil
}

func (p *PayOSAdapter) Get(ctx context.Context, orderCode int64) (*payos.PaymentLink, error) {
	ctx, cancel, capture := providerContext(ctx)
	defer cancel()
	result, err := p.sdk.PaymentRequests.Get(ctx, orderCode)
	if err != nil {
		return nil, capture.failure()
	}
	return result, nil
}

func (p *PayOSAdapter) Cancel(ctx context.Context, orderCode int64) (*payos.PaymentLink, error) {
	ctx, cancel, capture := providerContext(ctx)
	defer cancel()
	reason := "Cancelled by operator"
	result, err := p.sdk.PaymentRequests.Cancel(ctx, orderCode, &reason)
	if err != nil {
		return nil, capture.failure()
	}
	return result, nil
}

// Confirm is separate from the payment-link Provider contract. Its caller must use
// the configured canonical webhook URL, never one supplied by a browser.
func (p *PayOSAdapter) Confirm(ctx context.Context, webhookURL string) (string, error) {
	ctx, cancel, capture := providerContext(ctx)
	defer cancel()
	result, err := p.sdk.Webhooks.Confirm(ctx, webhookURL)
	if err != nil {
		return "", capture.failure()
	}
	return result, nil
}

func (p *PayOSAdapter) Verify(ctx context.Context, body map[string]any) (*payos.WebhookData, error) {
	ctx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, ok := body["signature"].(string)
	if !ok || len(signature) != 64 {
		return nil, ErrInvalidSignature
	}
	if _, err := hex.DecodeString(signature); err != nil {
		return nil, ErrInvalidSignature
	}
	data, ok := body["data"].(map[string]any)
	if !ok || data == nil || !validJSONNumbers(data) {
		return nil, ErrInvalidWebhook
	}
	if !safeNumber(data["orderCode"]) || !safeNumber(data["amount"]) {
		return nil, ErrInvalidWebhook
	}
	// Normalize validated safe integers before SDK signing/DTO decoding. Retain
	// every unknown field: only numeric representation changes, never data shape.
	encodedData, err := json.Marshal(data)
	if err != nil {
		return nil, ErrInvalidWebhook
	}
	var normalized map[string]any
	if err := json.Unmarshal(encodedData, &normalized); err != nil {
		return nil, ErrInvalidWebhook
	}
	verificationBody := map[string]any{"signature": signature, "data": normalized}
	verified, err := p.sdk.Webhooks.VerifyData(ctx, verificationBody)
	if err != nil {
		return nil, ErrInvalidSignature
	}
	encoded, err := json.Marshal(verified)
	if err != nil {
		return nil, ErrInvalidWebhook
	}
	var result payos.WebhookData
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, ErrInvalidWebhook
	}
	// Retain the complete signature-verified canonical object for inbox storage,
	// including extensions the SDK DTO cannot represent. Never replace on failure.
	body["data"] = normalized
	return &result, nil
}

func captureResponse(next payos.RequestHandler) payos.RequestHandler {
	return func(ctx context.Context, req *http.Request) (*http.Response, error) {
		capture, _ := ctx.Value(captureKey{}).(*responseCapture)
		if capture != nil {
			capture.status, capture.code, capture.transportFailure = 0, "", false
		}
		response, err := next(ctx, req)
		if err != nil {
			if capture != nil {
				capture.transportFailure = true
			}
			return response, err
		}
		if capture != nil {
			capture.status = response.StatusCode
			if delay := parseRetryAfter(response.Header.Get("Retry-After"), time.Now()); delay > capture.retryAfter {
				capture.retryAfter = delay
			}
		}
		original := response.Body
		body, readErr := io.ReadAll(io.LimitReader(original, maxProviderResponseBytes+1))
		original.Close()
		if readErr != nil || len(body) > maxProviderResponseBytes {
			if capture != nil {
				capture.transportFailure = true
			}
			return nil, errors.New("payment provider response cannot be read within limit")
		}
		var envelope struct {
			Code string          `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			if capture != nil {
				capture.code = sanitizedCode(envelope.Code)
			}
			// Validate exact JSON numbers before the SDK decodes into float64. This
			// never trusts or consumes business data before SDK signature verification.
			if len(envelope.Data) != 0 && !validEncodedNumbers(envelope.Data) {
				if capture != nil {
					capture.transportFailure = true
				}
				return nil, errors.New("payment provider response has invalid numbers")
			}
		}
		// Preserve all bytes (including whitespace and unknown fields) for the SDK.
		response.Body = io.NopCloser(bytes.NewReader(body))
		return response, nil
	}
}

func sanitizedCode(code string) string {
	if len(code) > 128 {
		return ""
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return ""
		}
	}
	return code
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

func validEncodedNumbers(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
		if number, ok := token.(json.Number); ok && !safeNumber(number) {
			return false
		}
	}
}

func validJSONNumbers(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			if !validJSONNumbers(child) {
				return false
			}
		}
		return true
	case []any:
		for _, child := range v {
			if !validJSONNumbers(child) {
				return false
			}
		}
		return true
	case nil, string, bool:
		return true
	default:
		return safeNumber(value)
	}
}

func safeNumber(value any) bool {
	switch v := value.(type) {
	case json.Number:
		if integer, err := v.Int64(); err == nil {
			return integer >= -maxSafeInteger && integer <= maxSafeInteger
		}
		// Use exact arithmetic: parsing 9007199254740991.1 as float64 would
		// silently round it into a permitted integer before signature verification.
		// Bound exact-arithmetic work as well as the surrounding HTTP body. An
		// attacker-controlled exponent must not allocate an enormous numerator.
		if len(v) > 128 {
			return false
		}
		if at := strings.IndexAny(string(v), "eE"); at >= 0 {
			exponent, err := strconv.ParseInt(string(v)[at+1:], 10, 32)
			if err != nil || exponent < -128 || exponent > 128 {
				return false
			}
		}
		rational, ok := new(big.Rat).SetString(string(v))
		return ok && rational.IsInt() && rational.Num().IsInt64() && safeNumber(rational.Num().Int64())
	case float64:
		return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v && v >= -float64(maxSafeInteger) && v <= float64(maxSafeInteger)
	case float32:
		return safeNumber(float64(v))
	case int:
		return safeNumber(int64(v))
	case int8:
		return true
	case int16:
		return true
	case int32:
		return true
	case int64:
		return v >= -maxSafeInteger && v <= maxSafeInteger
	case uint:
		return uint64(v) <= uint64(maxSafeInteger)
	case uint8:
		return true
	case uint16:
		return true
	case uint32:
		return true
	case uint64:
		return v <= uint64(maxSafeInteger)
	default:
		return false
	}
}
