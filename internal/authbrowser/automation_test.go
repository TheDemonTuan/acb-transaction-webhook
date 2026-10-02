package authbrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAutomationErrorsDoNotExposeSubmittedSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"fixture-password 001234 AB12CD"}`))
	}))
	defer upstream.Close()
	client := NewClient(upstream.URL)
	_, err := client.SubmitLogin(context.Background(), "attempt", LoginInput{Revision: "revision", Username: "fixture-user", Password: "fixture-password", AccountNumber: "222222222", Captcha: "AB12CD"})
	if !IsHTTPStatus(err, 409) {
		t.Fatal("conflict did not retain typed HTTP status")
	}
	for _, secret := range []string{"fixture-password", "001234", "AB12CD"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("automation error exposed response content")
		}
	}
}

func TestCaptureCaptchaRejectsOversizedOrNonPNG(t *testing.T) {
	for _, tc := range []struct{ name, body, contentType string }{
		{"oversized", "\x89PNG\r\n\x1a\n" + strings.Repeat("x", MaxCaptchaBytes), "image/png"},
		{"not PNG", "private-page-content", "image/png"},
		{"wrong MIME", "\x89PNG\r\n\x1a\n", "text/html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			if _, err := NewClient(upstream.URL).CaptureCaptcha(context.Background(), "attempt", "revision"); err == nil {
				t.Fatal("unsafe CAPTCHA transport response was accepted")
			}
		})
	}
}
