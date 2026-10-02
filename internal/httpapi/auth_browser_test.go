package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestManualAuthRoutesRemoved(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cutover.db"))
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
	before, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var browserCalls atomic.Int64
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		browserCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "VERIFIED", "session": "synthetic-handoff"})
	}))
	defer browser.Close()
	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner", AuthBrowserURL: browser.URL}, store).Handler()
	csrf := httptest.NewRecorder()
	h.ServeHTTP(csrf, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil))
	var token struct{ Token string }
	if err := json.Unmarshal(csrf.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	cookie := csrf.Result().Cookies()[0]
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/connection/auth/start"},
		{http.MethodGet, "/api/v1/connection/auth/current"},
		{http.MethodPost, "/api/v1/connection/auth/cancel"},
		{http.MethodGet, "/api/v1/connection/auth/" + attempt.ID + "/status"},
		{http.MethodGet, "/api/v1/connection/auth/" + attempt.ID + "/screen/vnc.html"},
		{http.MethodGet, "/api/v1/connection/auth/" + attempt.ID + "/screen/websockify"},
	} {
		t.Run(route.path, func(t *testing.T) {
			r := httptest.NewRequest(route.method, "http://example.test"+route.path, strings.NewReader(`{"attemptId":"`+attempt.ID+`"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "http://example.test")
			r.Header.Set("X-CSRF-Token", token.Token)
			r.AddCookie(cookie)
			if strings.HasSuffix(route.path, "/websockify") {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", "websocket")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("removed route returned %d: %s", w.Code, w.Body.String())
			}
		})
	}
	after, err := store.Connection(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("obsolete routes mutated connection: %+v %v", after, err)
	}
	current, err := store.AuthAttemptStatusForOwner(ctx, attempt.ID, "")
	if err != nil || !reflect.DeepEqual(attempt, current) {
		t.Fatalf("obsolete routes mutated attempt: %+v %v", current, err)
	}
	var attempts, sessions, runs int
	if err := store.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM auth_attempts),(SELECT count(*) FROM sessions),(SELECT count(*) FROM recovery_runs)`).Scan(&attempts, &sessions, &runs); err != nil {
		t.Fatal(err)
	}
	if browserCalls.Load() != 0 || attempts != 1 || sessions != 0 || runs != 0 {
		t.Fatalf("obsolete routes caused side effects: browser=%d attempts=%d sessions=%d recovery=%d", browserCalls.Load(), attempts, sessions, runs)
	}
}

func TestConnectionRecoveryStatusIsSanitizedAndReadOnly(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "connection-status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var browserCalls atomic.Int64
	browser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		browserCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "VERIFIED", "session": "synthetic-handoff"})
	}))
	defer browser.Close()
	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner", AuthBrowserURL: browser.URL}, store).Handler()
	get := func() map[string]json.RawMessage {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("connection status=%d %s", w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := get(); string(body["configured"]) != "false" || string(body["authRecovery"]) != "null" {
		t.Fatalf("unconfigured status=%s", body)
	}
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if body := get(); string(body["authRecovery"]) != "null" {
		t.Fatalf("status invented recovery episode: %s", body["authRecovery"])
	}
	attempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	episode, err := store.EnsureAuthRecoveryEpisode(ctx, connection.ID, connection.Generation)
	if err != nil {
		t.Fatal(err)
	}
	// An expired attempt must be observed, not expired/finalized by a dashboard GET.
	if _, err := store.DB().ExecContext(ctx, `UPDATE auth_attempts SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE auth_recovery_episodes SET attempt_id=?,reason_code='SESSION_INVALID',consent_action_id='synthetic-private-action' WHERE id=?`, attempt.ID, episode.ID); err != nil {
		t.Fatal(err)
	}
	episodeBefore, err := store.AuthRecoveryEpisode(ctx, episode.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		body := get()
		var recovery map[string]string
		if err := json.Unmarshal(body["authRecovery"], &recovery); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"state": episodeBefore.State, "reasonCode": "SESSION_INVALID", "updatedAt": episodeBefore.UpdatedAt}
		if !reflect.DeepEqual(recovery, want) {
			t.Fatalf("recovery response leaked private fields or changed state: %+v", recovery)
		}
	}
	connectionAfter, err := store.Connection(ctx)
	if err != nil || !reflect.DeepEqual(connection, connectionAfter) {
		t.Fatalf("dashboard GET mutated connection: %+v %v", connectionAfter, err)
	}
	episodeAfter, err := store.AuthRecoveryEpisode(ctx, episode.ID)
	if err != nil || !reflect.DeepEqual(episodeBefore, episodeAfter) {
		t.Fatalf("dashboard GET mutated recovery: %+v %v", episodeAfter, err)
	}
	var status string
	var sessions, runs int
	if err := store.DB().QueryRowContext(ctx, `SELECT status,(SELECT count(*) FROM sessions),(SELECT count(*) FROM recovery_runs) FROM auth_attempts WHERE id=?`, attempt.ID).Scan(&status, &sessions, &runs); err != nil {
		t.Fatal(err)
	}
	if status != "STARTING" || sessions != 0 || runs != 0 || browserCalls.Load() != 0 {
		t.Fatalf("dashboard GET advanced authentication: status=%s sessions=%d recovery=%d browser=%d", status, sessions, runs, browserCalls.Load())
	}
}
