package authrecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func fixtureLogout(t *testing.T, f *coordinatorFixture) storage.ACBLogoutJob {
	t.Helper()
	c, err := f.store.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.store.CreateTelegramAuthAction(f.ctx, storage.TelegramAuthAction{BotID: 1, ChatID: 22, UserID: 33, ExpectedGeneration: c.Generation, Action: "LOGOUT"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.DeliverTelegramAuthAction(f.ctx, a.ID, 79); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.store.ConsumeTelegramAuthAction(f.ctx, a.ID, 1, 22, 33, 79, time.Now()); err != nil {
		t.Fatal(err)
	}
	j, err := f.store.OpenACBLogoutJob(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestCoordinatorLogoutBankOnceWhileLocalOfflineThenRestartAck(t *testing.T) {
	f := newUnconsentedCoordinatorFixture(t)
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.store.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A live login page and an old stored session both remain available for revoke.
	attempt, err := f.store.StartAuthAttempt(f.ctx, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := "synthetic-revoke-handoff"
	envelope, err := keyring.Encrypt([]byte(plaintext), security.SessionAAD(c.ID, c.Generation))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES(?,?,?,'k1',?)`, c.ID, c.Generation, encoded, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	j := fixtureLogout(t, f)
	var revokeCalls, cancelCalls, localCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session-revocations":
			revokeCalls.Add(1)
			durable, err := f.store.ACBLogoutJob(f.ctx, j.ID)
			if err != nil || durable.BankStatus != "IN_FLIGHT" || durable.LocalClearedAt != "" {
				t.Errorf("bank claim/independent local result: %+v %v", durable, err)
			}
			if cancelCalls.Load() != 0 {
				t.Error("active browser cancelled before revoke")
			}
			var input struct {
				OperationID string `json:"operationId"`
				AttemptID   string `json:"attemptId"`
				Handoff     string `json:"handoff"`
			}
			if err = json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.OperationID != j.ID || input.AttemptID != attempt.ID || input.Handoff != plaintext {
				t.Error("revoke lost operation/active attempt/exact old-generation handoff")
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(authbrowser.RevocationResult{Status: "CONFIRMED", ReasonCode: "SESSION_REVOKED"})
		case r.Method == http.MethodDelete:
			cancelCalls.Add(1)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected bank request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	f.controller.Browser = authbrowser.NewClient(server.URL)
	f.controller.Keyring = keyring
	f.controller.InvalidateSession = func(ctx context.Context, id string, generation int64) error {
		localCalls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("unbounded worker invalidation")
		}
		if id != j.ConnectionID || generation != j.FencedGeneration {
			t.Error("wrong worker fence")
		}
		return errors.New("offline")
	}
	f.reconcile()
	after, err := f.store.ACBLogoutJob(f.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.BankStatus != "CONFIRMED" || after.State != "CLEARING" || after.LocalClearedAt != "" || after.SessionEnvelope != nil || revokeCalls.Load() != 1 || cancelCalls.Load() != 1 {
		t.Fatalf("independent outcomes: %+v bank=%d cleanup=%d", after, revokeCalls.Load(), cancelCalls.Load())
	}
	f.restart(false)
	f.controller.Browser = authbrowser.NewClient(server.URL)
	f.controller.Keyring = keyring
	f.controller.InvalidateSession = func(ctx context.Context, id string, generation int64) error {
		localCalls.Add(1)
		return f.store.CheckACBLogoutFence(ctx, id, generation)
	}
	f.reconcile()
	final, err := f.store.ACBLogoutJob(f.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "COMPLETED" || final.LocalClearedAt == "" || revokeCalls.Load() != 1 || localCalls.Load() != 2 {
		t.Fatalf("restart should retry local only: %+v bank=%d local=%d", final, revokeCalls.Load(), localCalls.Load())
	}
	if f.starts != 0 || f.logins != 0 || f.credentialReads != 0 {
		t.Fatal("logout started login/read credentials")
	}
}

func TestCoordinatorLogoutUnknownCrashNeverReplaysBankAndExpiresSnapshot(t *testing.T) {
	for _, scenario := range []string{"in-flight", "expired", "no-session"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUnconsentedCoordinatorFixture(t)
			if scenario != "no-session" {
				c, err := f.store.Connection(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.store.StartAuthAttempt(f.ctx, "owner", time.Minute); err != nil {
					t.Fatal(err)
				}
				if _, err = f.store.DB().Exec(`INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES(?,?,?,'k1',?)`, c.ID, c.Generation, []byte("encrypted"), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
			}
			j := fixtureLogout(t, f)
			if scenario == "in-flight" {
				if err := f.store.BeginACBLogoutRevocation(f.ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "expired" {
				f.now = f.now.Add(6 * time.Minute)
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(204)
					return
				}
				calls.Add(1)
				t.Errorf("unexpected bank replay %s", r.URL.Path)
				http.NotFound(w, r)
			}))
			defer server.Close()
			f.restart(false)
			f.controller.Browser = authbrowser.NewClient(server.URL)
			f.controller.InvalidateSession = func(ctx context.Context, id string, generation int64) error {
				return f.store.CheckACBLogoutFence(ctx, id, generation)
			}
			f.reconcile()
			final, err := f.store.ACBLogoutJob(f.ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			reason := "NO_SESSION_SNAPSHOT"
			if scenario == "in-flight" {
				reason = "LOGOUT_OUTCOME_UNKNOWN"
			}
			if scenario == "expired" {
				reason = "REVOKE_WINDOW_EXPIRED"
			}
			if calls.Load() != 0 || final.State != "LOCAL_ONLY" || final.BankStatus != "UNCONFIRMED" || final.BankReasonCode != reason || final.LocalClearedAt == "" || final.SessionEnvelope != nil {
				t.Fatalf("recovery=%+v calls=%d", final, calls.Load())
			}
		})
	}
}

func TestCoordinatorLogoutUnconfirmedIsLocalOnlyAndTransportFailureNotRetried(t *testing.T) {
	for _, scenario := range []string{"unsupported", "network"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUnconsentedCoordinatorFixture(t)
			if _, err := f.store.StartAuthAttempt(f.ctx, "owner", time.Minute); err != nil {
				t.Fatal(err)
			}
			j := fixtureLogout(t, f)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(204)
					return
				}
				calls.Add(1)
				if scenario == "network" {
					http.Error(w, "synthetic failure", 503)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(authbrowser.RevocationResult{Status: "UNCONFIRMED", ReasonCode: "LOGOUT_CONTROL_UNSUPPORTED"})
			}))
			defer server.Close()
			f.controller.Browser = authbrowser.NewClient(server.URL)
			f.controller.InvalidateSession = func(context.Context, string, int64) error { return nil }
			f.reconcile()
			final, err := f.store.ACBLogoutJob(f.ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			reason := "LOGOUT_CONTROL_UNSUPPORTED"
			if scenario == "network" {
				reason = "LOGOUT_OUTCOME_UNKNOWN"
			}
			if final.State != "LOCAL_ONLY" || final.BankStatus != "UNCONFIRMED" || final.BankReasonCode != reason || calls.Load() != 1 {
				t.Fatalf("result=%+v calls=%d", final, calls.Load())
			}
			f.reconcile()
			if calls.Load() != 1 {
				t.Fatal("terminal bank action replayed")
			}
		})
	}
}

func TestCoordinatorLogoutRevokesSupportedLegacyEnvelopeWithoutLiveAttempt(t *testing.T) {
	f := newUnconsentedCoordinatorFixture(t)
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.store.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := "legacy-synthetic-handoff"
	envelope, err := keyring.Encrypt([]byte(plaintext), security.LegacySessionAAD(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES(?,?,?,'k1',?)`, c.ID, c.Generation, encoded, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	j := fixtureLogout(t, f)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session-revocations" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var input struct {
			OperationID string `json:"operationId"`
			AttemptID   string `json:"attemptId"`
			Handoff     string `json:"handoff"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.OperationID != j.ID || input.AttemptID != "" || input.Handoff != plaintext {
			t.Error("legacy envelope was not decrypted for revoke")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(authbrowser.RevocationResult{Status: "CONFIRMED", ReasonCode: "SESSION_REVOKED"})
	}))
	defer server.Close()
	f.controller.Browser = authbrowser.NewClient(server.URL)
	f.controller.Keyring = keyring
	f.controller.InvalidateSession = func(context.Context, string, int64) error { return nil }
	f.reconcile()
	final, err := f.store.ACBLogoutJob(f.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || final.State != "COMPLETED" || final.BankStatus != "CONFIRMED" || final.SessionEnvelope != nil {
		t.Fatalf("legacy revoke result=%+v calls=%d", final, calls.Load())
	}
}
