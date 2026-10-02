package authbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

type AuthPageState string

const (
	LoginForm            AuthPageState = "LOGIN_FORM"
	CaptchaRequired      AuthPageState = "CAPTCHA_REQUIRED"
	OTPRequired          AuthPageState = "OTP_REQUIRED"
	Authenticated        AuthPageState = "AUTHENTICATED"
	LoginRejected        AuthPageState = "LOGIN_REJECTED"
	Maintenance          AuthPageState = "MAINTENANCE"
	Unknown              AuthPageState = "UNKNOWN"
	UnsupportedChallenge AuthPageState = "UNSUPPORTED_CHALLENGE"
	MaxCaptchaBytes                    = 512 << 10
)

type AuthObservation struct {
	State           AuthPageState `json:"state"`
	Revision        string        `json:"revision"`
	CaptchaRequired bool          `json:"captchaRequired"`
	ReasonCode      string        `json:"reasonCode,omitempty"`
	ExpiresAt       time.Time     `json:"expiresAt"`
}

type LoginInput struct {
	Revision      string `json:"revision"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	AccountNumber string `json:"accountNumber"`
	Captcha       string `json:"captcha,omitempty"`
}

type ChallengeInput struct {
	Revision string `json:"revision"`
	Value    string `json:"value"`
}

func (c *Client) Observe(ctx context.Context, attemptID string) (AuthObservation, error) {
	return c.automation(ctx, attemptID, "observation", nil)
}

func (c *Client) SubmitLogin(ctx context.Context, attemptID string, input LoginInput) (AuthObservation, error) {
	return c.automation(ctx, attemptID, "login", input)
}

func (c *Client) SubmitCaptcha(ctx context.Context, attemptID string, input ChallengeInput) (AuthObservation, error) {
	return c.automation(ctx, attemptID, "captcha", input)
}

func (c *Client) SubmitOTP(ctx context.Context, attemptID string, input ChallengeInput) (AuthObservation, error) {
	return c.automation(ctx, attemptID, "otp", input)
}

func (c *Client) automation(ctx context.Context, attemptID, action string, input any) (AuthObservation, error) {
	var body io.Reader
	method := http.MethodGet
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil || len(payload) > 8<<10 {
			return AuthObservation{}, errors.New("invalid browser action")
		}
		body = bytes.NewReader(payload)
		method = http.MethodPost
	}
	request, err := c.request(ctx, method, "/sessions/"+url.PathEscape(attemptID)+"/"+action, body)
	if err != nil {
		return AuthObservation{}, errors.New("invalid browser request")
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return AuthObservation{}, errors.New("browser automation unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return AuthObservation{}, &HTTPError{StatusCode: response.StatusCode}
	}
	var observation AuthObservation
	if decodeJSON(response.Body, &observation) != nil || observation.State == "" || observation.Revision == "" || observation.ExpiresAt.IsZero() {
		return AuthObservation{}, errors.New("invalid browser observation")
	}
	return observation, nil
}

func (c *Client) CaptureCaptcha(ctx context.Context, attemptID, revision string) ([]byte, error) {
	request, err := c.request(ctx, http.MethodGet, "/sessions/"+url.PathEscape(attemptID)+"/captcha?revision="+url.QueryEscape(revision), nil)
	if err != nil {
		return nil, errors.New("invalid browser request")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, errors.New("browser CAPTCHA unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPError{StatusCode: response.StatusCode}
	}
	png, err := io.ReadAll(io.LimitReader(response.Body, MaxCaptchaBytes+1))
	if err != nil || len(png) > MaxCaptchaBytes || !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) || response.Header.Get("Content-Type") != "image/png" {
		return nil, errors.New("invalid browser CAPTCHA image")
	}
	return png, nil
}
