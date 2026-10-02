package authbrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientRejectsHTMLUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<!DOCTYPE html><title>Cloudflare 502</title>"))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Status(context.Background(), "auth_1")
	if err == nil {
		t.Fatal("expected an upstream error")
	}
	if !IsHTTPStatus(err, http.StatusBadGateway) {
		t.Fatalf("expected HTTP 502, got %v", err)
	}
	if strings.Contains(err.Error(), "DOCTYPE") {
		t.Fatalf("HTML leaked into error: %v", err)
	}
}

func TestClientUsesJSONUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"browser startup timed out"}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Start(context.Background(), "auth_1")
	if err == nil || !strings.Contains(err.Error(), "browser startup timed out") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientRejectsIncompleteSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"attemptId":"auth_1"}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL).Status(context.Background(), "auth_1")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientSendsInternalToken(t *testing.T) {
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get(InternalTokenHeader)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"attemptId": "auth_1",
			"status":    "AWAITING_USER_LOGIN",
		})
	}))
	defer server.Close()

	if _, err := NewClient(server.URL, "internal-secret").Status(context.Background(), "auth_1"); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if gotToken != "internal-secret" {
		t.Fatalf("internal token header = %q, want internal-secret", gotToken)
	}
}

func TestRevocationRejectsUntrustedResultsWithoutLeakingPayload(t *testing.T) {
	for _, body := range []string{
		`{"status":"CONFIRMED","reasonCode":"LOGOUT_NO_SESSION"}`,
		`{"status":"UNCONFIRMED","reasonCode":"SESSION_REVOKED"}`,
		`{"status":"UNKNOWN","reasonCode":"LOGOUT_OUTCOME_UNKNOWN"}`,
		`{"status":"CONFIRMED","reasonCode":"secret-cookie-value"}`,
		`{"status":"CONFIRMED","reasonCode":"SESSION_REVOKED"} trailing secret-cookie-value`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		result, err := NewClient(server.URL, "fixture-internal").RevokeSession(context.Background(), "logout", "", "")
		server.Close()
		if err == nil || result.Status != "" || strings.Contains(err.Error(), "secret-cookie-value") {
			t.Fatalf("untrusted result escaped boundary: %+v %v", result, err)
		}
	}
}

func TestRevocationDoesNotFollowRedirectOrReplayUnknownOutcome(t *testing.T) {
	calls, foreignCalls := 0, 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls++ }))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "fixture-internal").RevokeSession(context.Background(), "logout", "", "synthetic-snapshot")
	if !IsHTTPStatus(err, 307) || calls != 1 || foreignCalls != 0 {
		t.Fatalf("revocation redirected/replayed: calls=%d foreign=%d err=%v", calls, foreignCalls, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewClient(server.URL, "fixture-internal").RevokeSession(cancelled, "logout", "", "synthetic-snapshot")
	if err == nil || calls != 1 || strings.Contains(err.Error(), "synthetic-snapshot") {
		t.Fatal("cancelled revocation was dispatched or leaked its snapshot")
	}
}
