package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type acceptingAuthVerifier struct{}

func (acceptingAuthVerifier) VerifySession(context.Context, string, int64, []byte) error { return nil }

type recoveryRecorder struct {
	calls        int
	connectionID string
	generation   int64
	eventKey     string
}

func (r *recoveryRecorder) ScheduleRecovery(_ context.Context, connectionID string, generation int64, eventKey string) error {
	r.calls++
	r.connectionID = connectionID
	r.generation = generation
	r.eventKey = eventKey
	return nil
}

type rejectingAuthVerifier struct{ err error }

func (v rejectingAuthVerifier) VerifySession(context.Context, string, int64, []byte) error {
	return v.err
}

func TestAuthBrowserStatusFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upstream  int
		status    string
		wantHTTP  int
		wantState string
	}{
		{"missing browser", 404, "", 200, "AUTH_REQUIRED"},
		{"failed browser", 200, "FAILED", 200, "AUTH_REQUIRED"},
		{"expired browser", 200, "EXPIRED", 200, "AUTH_REQUIRED"},
		{"cancelled browser", 200, "CANCELLED", 200, "AUTH_REQUIRED"},
		{"temporary upstream failure", 503, "", 502, "AUTH_STARTING"},
		{"verified without keyring", 200, "VERIFIED", 503, "AUTH_STARTING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err = store.ConfigureConnection(ctx, "***1234"); err != nil {
				t.Fatal(err)
			}
			attempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			handoffs := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					handoffs++
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstream)
				_ = json.NewEncoder(w).Encode(map[string]string{"attemptId": attempt.ID, "status": tc.status})
			}))
			defer upstream.Close()
			server := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner", AuthBrowserURL: upstream.URL}, store)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/status", nil))
			if w.Code != tc.wantHTTP {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			connection, err := store.Connection(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if connection.State != tc.wantState {
				t.Fatalf("connection state %s, want %s", connection.State, tc.wantState)
			}
			if handoffs != 0 {
				t.Fatal("consumed one-time handoff without an encryption key")
			}
			if tc.wantState == "AUTH_REQUIRED" {
				if _, err = store.StartAuthAttempt(ctx, "", time.Minute); err != nil {
					t.Fatalf("cannot retry after terminal browser failure: %v", err)
				}
			}
		})
	}
}

func TestAuthBrowserStatusTerminalIdempotence(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, []byte(base64.RawStdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/handoff") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"session": "handoff-secret-data"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"attemptId": attempt.ID, "status": "VERIFIED"})
	}))
	defer upstream.Close()

	recovery := &recoveryRecorder{}
	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
		MasterKeyFile:      keyPath,
	}, store).WithAuthVerifier(acceptingAuthVerifier{}).WithPostAuthRecoveryRequester(recovery)

	// First poll transitions to MONITORING
	w1 := httptest.NewRecorder()
	server.Handler().ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/status", nil))
	if w1.Code != http.StatusOK {
		t.Fatalf("first poll status %d: %s", w1.Code, w1.Body.String())
	}
	var res1 map[string]string
	_ = json.Unmarshal(w1.Body.Bytes(), &res1)
	if res1["status"] != "MONITORING" {
		t.Fatalf("first poll status=%v, want MONITORING", res1["status"])
	}
	if recovery.calls != 1 || recovery.connectionID != attempt.ConnectionID || recovery.generation != attempt.Generation || recovery.eventKey != attempt.ID {
		t.Fatalf("recovery hook = %+v, want one call for completed auth", recovery)
	}

	// Subsequent poll returns MONITORING idempotently (NOT 404!)
	w2 := httptest.NewRecorder()
	server.Handler().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/status", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("second poll status %d: %s", w2.Code, w2.Body.String())
	}
	var res2 map[string]string
	_ = json.Unmarshal(w2.Body.Bytes(), &res2)
	if res2["status"] != "MONITORING" {
		t.Fatalf("second poll status=%v, want MONITORING", res2["status"])
	}
	if recovery.calls != 1 {
		t.Fatalf("recovery hook called %d times after repeated status, want 1", recovery.calls)
	}

	// VNC screen access must be rejected after terminal status
	wScreen := httptest.NewRecorder()
	server.Handler().ServeHTTP(wScreen, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/screen/vnc.html", nil))
	if wScreen.Code != http.StatusNotFound {
		t.Fatalf("VNC screen allowed after terminal: got %d", wScreen.Code)
	}
}

func TestAuthBrowserVerificationFailureNeverEntersMonitoring(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, []byte(base64.RawStdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	cancelled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/handoff"):
			_ = json.NewEncoder(w).Encode(map[string]string{"session": "handoff-secret-data"})
		case r.Method == http.MethodDelete:
			cancelled++
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"attemptId": attempt.ID, "status": "VERIFIED"})
		}
	}))
	defer upstream.Close()
	server := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner", AuthBrowserURL: upstream.URL, MasterKeyFile: keyPath}, store).
		WithAuthVerifier(rejectingAuthVerifier{err: errors.New("login page")})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/status", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"VERIFYING"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	connection, err := store.Connection(ctx)
	if err != nil || connection.State == "MONITORING" {
		t.Fatalf("connection entered monitoring on failed verification: %+v err=%v", connection, err)
	}
	if cancelled != 0 {
		t.Fatalf("browser should NOT be cancelled on retryable verification failure, got %d", cancelled)
	}
}

func TestAuthBrowserStatusTTLExpiryLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	// Start attempt with short TTL
	attempt, err := store.StartAuthAttempt(ctx, "", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"attemptId": attempt.ID, "status": "IN_PROGRESS"})
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
	}, store)

	// First poll detects expiry
	w1 := httptest.NewRecorder()
	server.Handler().ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/status", nil))
	if w1.Code != http.StatusOK {
		t.Fatalf("expiry poll status %d: %s", w1.Code, w1.Body.String())
	}
	var res1 map[string]string
	_ = json.Unmarshal(w1.Body.Bytes(), &res1)
	if res1["status"] != "EXPIRED" || !strings.Contains(res1["error"], "Phiên đăng nhập ACB đã hết hạn") {
		t.Fatalf("expected EXPIRED status and message, got %+v", res1)
	}

	conn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if conn.State != "AUTH_REQUIRED" {
		t.Fatalf("expected state AUTH_REQUIRED, got %s", conn.State)
	}

	// Subsequent poll must NOT return 404; must return EXPIRED idempotently
	w2 := httptest.NewRecorder()
	server.Handler().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/status", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("second expiry poll returned %d (wanted 200, NOT 404): %s", w2.Code, w2.Body.String())
	}
	var res2 map[string]string
	_ = json.Unmarshal(w2.Body.Bytes(), &res2)
	if res2["status"] != "EXPIRED" {
		t.Fatalf("expected second poll status EXPIRED, got %+v", res2)
	}

	// VNC screen access must return 404
	wScreen := httptest.NewRecorder()
	server.Handler().ServeHTTP(wScreen, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/screen/vnc.html", nil))
	if wScreen.Code != http.StatusNotFound {
		t.Fatalf("VNC screen allowed after expiry: got %d", wScreen.Code)
	}
}

func TestAuthBrowserCancelEndpoint(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	cancelShouldFail := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if cancelShouldFail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"attemptId": "test", "status": "IN_PROGRESS"})
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
	}, store)
	h := server.Handler()

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// 1. Ownership enforcement: attempt owned by another subject
	otherAttempt, err := store.StartAuthAttempt(ctx, "other_owner@example.com", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Dev caller has identity.Email == "" which does not match "other_owner@example.com"
	wWrongOwner := post("/api/v1/connection/auth/cancel", `{"attemptId":"`+otherAttempt.ID+`"}`)
	if wWrongOwner.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for wrong owner, got %d: %s", wWrongOwner.Code, wWrongOwner.Body.String())
	}

	// Finish otherAttempt so we can start our own
	if err := store.FinishAuthAttempt(ctx, otherAttempt.ID, "FAILED"); err != nil {
		t.Fatal(err)
	}

	// 2. Upstream cancel failure: must not claim success or finish attempt
	myAttempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	cancelShouldFail = true
	wFail := post("/api/v1/connection/auth/cancel", `{"attemptId":"`+myAttempt.ID+`"}`)
	if wFail.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway when upstream cancel fails, got %d: %s", wFail.Code, wFail.Body.String())
	}
	// Verify attempt is still active in DB
	activeAttempt, err := store.AuthAttemptStatusForOwner(ctx, myAttempt.ID, "")
	if err != nil || activeAttempt.Status != "STARTING" {
		t.Fatalf("attempt should remain STARTING after failed cancel: %+v, %v", activeAttempt, err)
	}

	// 3. Successful cancel
	cancelShouldFail = false
	wSuccess := post("/api/v1/connection/auth/cancel", `{"attemptId":"`+myAttempt.ID+`"}`)
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected 200 for successful cancel, got %d: %s", wSuccess.Code, wSuccess.Body.String())
	}
	cancelledAttempt, err := store.AuthAttemptStatusForOwner(ctx, myAttempt.ID, "")
	if err != nil || cancelledAttempt.Status != "CANCELLED" {
		t.Fatalf("attempt should be CANCELLED: %+v, %v", cancelledAttempt, err)
	}

	// 4. Repeated cancel is idempotent
	wRepeat := post("/api/v1/connection/auth/cancel", `{"attemptId":"`+myAttempt.ID+`"}`)
	if wRepeat.Code != http.StatusOK {
		t.Fatalf("expected 200 for idempotent cancel, got %d: %s", wRepeat.Code, wRepeat.Body.String())
	}

	// 5. Status poll on cancelled attempt returns CANCELLED (not 404)
	wStatus := httptest.NewRecorder()
	h.ServeHTTP(wStatus, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+myAttempt.ID+"/status", nil))
	if wStatus.Code != http.StatusOK {
		t.Fatalf("expected 200 for status of cancelled attempt, got %d: %s", wStatus.Code, wStatus.Body.String())
	}
	var res map[string]string
	_ = json.Unmarshal(wStatus.Body.Bytes(), &res)
	if res["status"] != "CANCELLED" {
		t.Fatalf("expected status CANCELLED, got %+v", res)
	}

	// 6. VNC screen access returns 404
	wScreen := httptest.NewRecorder()
	h.ServeHTTP(wScreen, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+myAttempt.ID+"/screen/vnc.html", nil))
	if wScreen.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for VNC screen of cancelled attempt, got %d", wScreen.Code)
	}
}

func TestAuthBrowserStaleResponseSuperseded(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	attempt1, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"attemptId": attempt1.ID, "status": "VERIFIED"})
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
	}, store)

	// Transition connection to pause (bumps connection generation from 2 to 3)
	pausedConn, err := store.TransitionConnection(ctx, "pause")
	if err != nil {
		t.Fatal(err)
	}
	if pausedConn.Generation <= attempt1.Generation {
		t.Fatalf("expected paused generation > attempt1.Generation: %d <= %d", pausedConn.Generation, attempt1.Generation)
	}

	// Check status for attempt 1 (generation 2) when connection is now generation 3
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt1.ID+"/status", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for superseded attempt, got %d: %s", w.Code, w.Body.String())
	}

	// Verify connection generation and state was NOT corrupted/reverted
	connAfter, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if connAfter.Generation != pausedConn.Generation || connAfter.State != "PAUSED" {
		t.Fatalf("connection corrupted by stale response: %+v", connAfter)
	}
}

func TestAuthBrowserCurrentAndResumeIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attemptId": "auth_test_resume",
				"status":    "AWAITING_USER_LOGIN",
				"screenUrl": "/",
				"expiresAt": time.Now().Add(15 * time.Minute).Format(time.RFC3339),
			})
			return
		}
		// GET status
		_ = json.NewEncoder(w).Encode(map[string]string{
			"attemptId": "auth_test_resume",
			"status":    "AWAITING_USER_LOGIN",
			"screenUrl": "/",
			"expiresAt": time.Now().Add(15 * time.Minute).Format(time.RFC3339),
		})
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
	}, store)
	h := server.Handler()

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)

	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// 1. Initially, GET /current returns { "attempt": null }
	wCurrent1 := httptest.NewRecorder()
	h.ServeHTTP(wCurrent1, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/current", nil))
	if wCurrent1.Code != http.StatusOK || !strings.Contains(wCurrent1.Body.String(), `"attempt":null`) {
		t.Fatalf("expected attempt:null, got: %s", wCurrent1.Body.String())
	}

	// 2. Start an auth session -> 201 Created
	wStart1 := post("/api/v1/connection/auth/start", `{}`)
	if wStart1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", wStart1.Code, wStart1.Body.String())
	}

	// 3. GET /current now recovers the active attempt and screenUrl!
	wCurrent2 := httptest.NewRecorder()
	h.ServeHTTP(wCurrent2, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/current", nil))
	if wCurrent2.Code != http.StatusOK || !strings.Contains(wCurrent2.Body.String(), `vnc.html`) {
		t.Fatalf("expected current attempt with vnc.html, got: %s", wCurrent2.Body.String())
	}

	// 4. Repeated POST /start while browser is active RESUMES existing session with 200 OK (no UNIQUE constraint failure!)
	wStart2 := post("/api/v1/connection/auth/start", `{}`)
	if wStart2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on resume, got %d: %s", wStart2.Code, wStart2.Body.String())
	}
	if !strings.Contains(wStart2.Body.String(), `vnc.html`) {
		t.Fatalf("expected screenUrl in resumed response, got: %s", wStart2.Body.String())
	}
}

func TestStartAuthTransientFailurePreservesAttempt(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	upstreamStatus := http.StatusServiceUnavailable
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attemptId": "auth_test_transient",
				"status":    "AWAITING_USER_LOGIN",
				"screenUrl": "/",
				"expiresAt": time.Now().Add(15 * time.Minute).Format(time.RFC3339),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamStatus)
		if upstreamStatus == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attemptId": "auth_test_transient",
				"status":    "AWAITING_USER_LOGIN",
				"screenUrl": "/",
				"expiresAt": time.Now().Add(15 * time.Minute).Format(time.RFC3339),
			})
		}
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
	}, store)
	h := server.Handler()

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)

	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// 1. First start -> 201 Created
	wStart1 := post("/api/v1/connection/auth/start", `{}`)
	if wStart1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", wStart1.Code, wStart1.Body.String())
	}

	active1, found1, err := store.ActiveAuthAttemptForOwner(ctx, "")
	if err != nil || !found1 {
		t.Fatalf("expected active attempt in DB, err=%v", err)
	}

	// 2. Upstream status experiences transient 503 error
	upstreamStatus = http.StatusServiceUnavailable
	wStartTransient := post("/api/v1/connection/auth/start", `{}`)
	if wStartTransient.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for transient browser failure, got %d: %s", wStartTransient.Code, wStartTransient.Body.String())
	}

	// Active attempt must NOT be marked FAILED
	active2, found2, err := store.ActiveAuthAttemptForOwner(ctx, "")
	if err != nil || !found2 {
		t.Fatalf("active attempt must be preserved across transient errors, err=%v", err)
	}
	if active2.ID != active1.ID || active2.Status != "IN_PROGRESS" {
		t.Fatalf("attempt state corrupted: %+v", active2)
	}

	// 3. Upstream recovers -> resume succeeds with 200 OK
	upstreamStatus = http.StatusOK
	wStartRecovered := post("/api/v1/connection/auth/start", `{}`)
	if wStartRecovered.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after recovery, got %d: %s", wStartRecovered.Code, wStartRecovered.Body.String())
	}

	// 4. Upstream 404 (session genuinely gone) -> marks FAILED and starts new attempt (201 Created)
	upstreamStatus = http.StatusNotFound
	wStart404 := post("/api/v1/connection/auth/start", `{}`)
	if wStart404.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created after 404 cleanup, got %d: %s", wStart404.Code, wStart404.Body.String())
	}
}

func TestAuthBrowserCancelPersistsWhenBrowserReturns404(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cancel_404.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sessions" {
			var body struct {
				AttemptID string `json:"attemptId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attemptId": body.AttemptID,
				"status":    "STARTING",
				"screenUrl": "/",
				"expiresAt": time.Now().Add(15 * time.Minute).Format(time.RFC3339),
			})
			return
		}
		if r.Method == http.MethodDelete {
			// Upstream browser already lost/deleted session, returns 404
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "session not found"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
	}, store)
	h := server.Handler()

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)

	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// 1. Start auth attempt
	wStart := post("/api/v1/connection/auth/start", `{}`)
	if wStart.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", wStart.Code, wStart.Body.String())
	}
	var startResp struct {
		AttemptID string `json:"attemptId"`
	}
	_ = json.NewDecoder(wStart.Body).Decode(&startResp)

	// 2. Explicit cancel while upstream returns 404
	wCancel := post("/api/v1/connection/auth/cancel", `{"attemptId":"`+startResp.AttemptID+`"}`)
	if wCancel.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for cancel despite upstream 404, got %d: %s", wCancel.Code, wCancel.Body.String())
	}

	// 3. Store must persist CANCELLED status
	status, err := store.AuthAttemptStatusForOwner(ctx, startResp.AttemptID, "")
	if err != nil {
		t.Fatalf("AuthAttemptStatusForOwner: %v", err)
	}
	if status.Status != "CANCELLED" {
		t.Fatalf("expected attempt status CANCELLED, got %s", status.Status)
	}
}

func TestAuthBrowserHandoffGenerationMismatchRejectsStaleVerification(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "stale_verify.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}

	attempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, []byte(base64.RawStdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/handoff") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"session": "handoff-secret-data"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"attemptId": attempt.ID,
			"status":    "VERIFIED",
		})
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
		MasterKeyFile:      keyPath,
	}, store).WithAuthVerifier(rejectingAuthVerifier{err: errors.New("should not be called")})

	// Bump connection generation behind the scenes before verification
	_, err = store.DB().ExecContext(ctx, `UPDATE connections SET generation = generation + 1 WHERE id = ?`, conn.ID)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connection/auth/"+attempt.ID+"/status", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for stale generation handoff, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AUTH_SESSION_SUPERSEDED") {
		t.Fatalf("expected AUTH_SESSION_SUPERSEDED, got: %s", w.Body.String())
	}

	// Verify connection state did NOT transition to MONITORING
	latestConn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latestConn.State == "MONITORING" {
		t.Fatalf("connection must not adopt stale session, state is %s", latestConn.State)
	}
}

func TestAuthBrowserConcurrentStartLoginRace(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "concurrent_start.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attemptId": "concurrent_test",
				"status":    "STARTING",
				"screenUrl": "/",
				"expiresAt": time.Now().Add(15 * time.Minute).Format(time.RFC3339),
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"attemptId": "concurrent_test",
			"status":    "STARTING",
			"screenUrl": "/",
			"expiresAt": time.Now().Add(15 * time.Minute).Format(time.RFC3339),
		})
	}))
	defer upstream.Close()

	server := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserURL:     upstream.URL,
	}, store)
	h := server.Handler()

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)

	const concurrency = 8
	var wg sync.WaitGroup
	codes := make([]int, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/connection/auth/start", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "http://example.test")
			r.Header.Set("X-CSRF-Token", token.Token)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			codes[idx] = w.Code
		}(i)
	}
	wg.Wait()

	createdCount := 0
	okOrConflictCount := 0
	for _, code := range codes {
		if code == http.StatusCreated {
			createdCount++
		} else if code == http.StatusConflict || code == http.StatusOK {
			okOrConflictCount++
		} else {
			t.Errorf("unexpected status code: %d", code)
		}
	}

	if createdCount != 1 {
		t.Fatalf("expected exactly 1 attempt created (201), got %d (all codes: %v)", createdCount, codes)
	}
	if okOrConflictCount != concurrency-1 {
		t.Fatalf("expected %d conflict or resume responses, got %d (all codes: %v)", concurrency-1, okOrConflictCount, codes)
	}
}
