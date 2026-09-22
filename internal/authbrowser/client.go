package authbrowser

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

const (
	maxResponseBody     = 64 << 10
	InternalTokenHeader = "X-Auth-Browser-Internal-Token"
)

type Session struct {
	AttemptID string `json:"attemptId"`
	Status    string `json:"status"`
	ScreenURL string `json:"screenUrl"`
	ExpiresAt string `json:"expiresAt"`
	Error     string `json:"error,omitempty"`
}

type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("ACB browser returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("ACB browser returned HTTP %d: %s", e.StatusCode, e.Message)
}

func IsHTTPStatus(err error, status int) bool {
	var responseErr *HTTPError
	return errors.As(err, &responseErr) && responseErr.StatusCode == status
}

type Client struct {
	baseURL       string
	internalToken string
	http          *http.Client
}

func NewClient(baseURL string, internalToken ...string) *Client {
	token := ""
	if len(internalToken) > 0 {
		token = strings.TrimSpace(internalToken[0])
	}
	return &Client{
		baseURL:       strings.TrimSuffix(baseURL, "/"),
		internalToken: token,
		http:          &http.Client{Timeout: 12 * time.Second},
	}
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.internalToken != "" {
		request.Header.Set(InternalTokenHeader, c.internalToken)
	}
	return request, nil
}

func (c *Client) Start(ctx context.Context, attemptID string) (Session, error) {
	body, _ := json.Marshal(map[string]string{"attemptId": attemptID})
	request, err := c.request(ctx, http.MethodPost, "/sessions", bytes.NewReader(body))
	if err != nil {
		return Session{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	return c.sessionResponse(request, http.StatusCreated, "start")
}

func (c *Client) Cancel(ctx context.Context, attemptID string) error {
	request, err := c.request(ctx, http.MethodDelete, "/sessions/"+attemptID, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("cancel ACB browser: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	return nil
}

func (c *Client) Status(ctx context.Context, attemptID string) (Session, error) {
	request, err := c.request(ctx, http.MethodGet, "/sessions/"+attemptID+"/status", nil)
	if err != nil {
		return Session{}, err
	}
	return c.sessionResponse(request, http.StatusOK, "read status")
}

func (c *Client) Handoff(ctx context.Context, attemptID string) (string, error) {
	request, err := c.request(ctx, http.MethodPost, "/sessions/"+attemptID+"/handoff", bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("handoff ACB browser: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", responseError(response)
	}
	var body struct {
		Session string `json:"session"`
	}
	if err := decodeJSON(response.Body, &body); err != nil {
		return "", fmt.Errorf("decode ACB browser handoff: %w", err)
	}
	if body.Session == "" {
		return "", errors.New("ACB browser handoff is empty")
	}
	return body.Session, nil
}

func (c *Client) Complete(ctx context.Context, attemptID string) error {
	request, err := c.request(ctx, http.MethodPost, "/sessions/"+attemptID+"/complete", bytes.NewReader(nil))
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("complete ACB browser handoff: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return responseError(response)
	}
	return nil
}

func (c *Client) sessionResponse(request *http.Request, expected int, operation string) (Session, error) {
	response, err := c.http.Do(request)
	if err != nil {
		return Session{}, fmt.Errorf("%s ACB browser: %w", operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode != expected && !(expected == http.StatusCreated && response.StatusCode == http.StatusOK) {
		return Session{}, responseError(response)
	}
	var session Session
	if err := decodeJSON(response.Body, &session); err != nil {
		return Session{}, fmt.Errorf("decode ACB browser response: %w", err)
	}
	if session.AttemptID == "" || session.Status == "" {
		return Session{}, errors.New("ACB browser response is incomplete")
	}
	return session, nil
}

func responseError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return &HTTPError{StatusCode: response.StatusCode}
	}
	var payload struct {
		Error string `json:"error"`
	}
	message := ""
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") && json.Unmarshal(body, &payload) == nil {
		message = strings.TrimSpace(payload.Error)
	}
	return &HTTPError{StatusCode: response.StatusCode, Message: message}
}

func decodeJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxResponseBody))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("response contains trailing data")
	}
	return nil
}
