package authsession

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type browserFixture struct {
	handoffs      int
	completions   int
	handoffError  error
	completeError error
	afterHandoff  func()
}

func (b *browserFixture) Handoff(context.Context, string) (string, error) {
	b.handoffs++
	if b.afterHandoff != nil {
		b.afterHandoff()
	}
	return "cookie=synthetic-secret; OTP=001234", b.handoffError
}
func (b *browserFixture) Complete(context.Context, string) error {
	b.completions++
	return b.completeError
}

type verifierFunc func(context.Context, string, int64, []byte) error

func (v verifierFunc) VerifySession(ctx context.Context, id string, generation int64, encrypted []byte) error {
	return v(ctx, id, generation, encrypted)
}

type recoveryScheduler struct {
	store     *storage.Store
	available bool
	runIDs    []string
}

func (s *recoveryScheduler) ScheduleRecovery(ctx context.Context, id string, generation int64, eventKey string) error {
	if !s.available {
		return errors.New("scheduler transport unavailable")
	}
	run, err := s.store.GetRecoveryRunByEvent(ctx, id, generation, eventKey)
	if err != nil {
		return err
	}
	s.runIDs = append(s.runIDs, run.ID)
	return nil
}

func finalizerStore(t *testing.T) (*storage.Store, *security.Keyring, storage.AuthAttempt) {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "finalizer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err = s.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.StartAuthAttempt(ctx, "operator", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return s, keyring, a
}

func TestRecoveryFinalizeCrashBoundaries(t *testing.T) {
	ctx := context.Background()
	s, keyring, previous := finalizerStore(t)
	if _, err := s.CompleteAuthSession(ctx, previous.ID, []byte("previous-encrypted-session")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: previous.ConnectionID, ScanID: "previous-scan", CoverageFrom: "2026-09-28", CoverageTo: "2026-09-30"}); err != nil {
		t.Fatal(err)
	}
	cpBefore, err := s.GetCheckpoint(ctx, previous.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	oldRun, err := s.GetRecoveryRunByEvent(ctx, previous.ConnectionID, previous.Generation, previous.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.StartAuthAttempt(ctx, "operator", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	browser := &browserFixture{completeError: errors.New("lost response after browser cleanup")}
	scheduler := &recoveryScheduler{store: s}
	verifications := 0
	reject := true
	verify := verifierFunc(func(_ context.Context, id string, generation int64, encrypted []byte) error {
		verifications++
		var envelope security.Envelope
		if err := json.Unmarshal(encrypted, &envelope); err != nil {
			t.Fatal(err)
		}
		plain, err := keyring.Decrypt(envelope, security.SessionAAD(id, generation))
		if err != nil || string(plain) != "cookie=synthetic-secret; OTP=001234" {
			t.Fatalf("incorrect encrypted handoff: %v", err)
		}
		if reject {
			return errors.New("cookie=synthetic-secret; OTP=001234")
		}
		return nil
	})
	f := NewFinalizer(Options{Store: s, Browser: browser, Keyring: keyring, Verifier: verify, Scheduler: scheduler})
	if _, err := f.Complete(ctx, a); !errors.Is(err, ErrVerificationPending) {
		t.Fatalf("expected retryable verification, got %v", err)
	}
	oldSession, err := s.Session(ctx, previous.ConnectionID, previous.Generation)
	if err != nil || string(oldSession.Envelope) != "previous-encrypted-session" {
		t.Fatalf("rejected verification replaced prior session: %v", err)
	}
	cpAfter, err := s.GetCheckpoint(ctx, a.ConnectionID)
	if err != nil || !reflect.DeepEqual(cpBefore, cpAfter) {
		t.Fatalf("rejection changed checkpoint: %v", err)
	}
	if _, err := s.GetRecoveryRunByEvent(ctx, a.ConnectionID, a.Generation, a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejection created recovery intent: %v", err)
	}
	var priorRunStatus string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM recovery_runs WHERE id=?`, oldRun.ID).Scan(&priorRunStatus); err != nil || priorRunStatus != string(oldRun.Status) {
		t.Fatalf("rejection modified prior run: %v", err)
	}

	reject = false
	conn, err := f.Complete(ctx, a)
	if err != nil || conn.State != "MONITORING" {
		t.Fatalf("cleanup failure rolled back verified commit: %v", err)
	}
	run, err := s.GetRecoveryRunByEvent(ctx, a.ConnectionID, a.Generation, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheduler.available = true
	// Simulate a restarted caller holding the pre-commit IN_PROGRESS/STARTING snapshot.
	f = NewFinalizer(Options{Store: s, Browser: browser, Scheduler: scheduler})
	if _, err := f.Complete(ctx, a); err != nil {
		t.Fatal(err)
	}
	if browser.handoffs != 2 || verifications != 2 {
		t.Fatal("committed session requested another browser handoff or verification")
	}
	if browser.completions != 2 {
		t.Fatal("restart did not retry browser cleanup")
	}
	if !reflect.DeepEqual(scheduler.runIDs, []string{run.ID}) {
		t.Fatalf("restart failed to schedule original durable intent: %v", scheduler.runIDs)
	}
	var count int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM recovery_runs WHERE connection_id=? AND generation=?`, a.ConnectionID, a.Generation).Scan(&count); err != nil || count != 1 {
		t.Fatalf("restart duplicated intent: count=%d err=%v", count, err)
	}
	cpAfter, err = s.GetCheckpoint(ctx, a.ConnectionID)
	if err != nil || !reflect.DeepEqual(cpBefore, cpAfter) {
		t.Fatalf("commit changed checkpoint: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE connections SET generation=generation+1,config_revision=config_revision+1,state='AUTH_REQUIRED' WHERE id=?`, a.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Complete(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("adopted superseded VERIFIED attempt: %v", err)
	}
	if browser.completions != 2 || len(scheduler.runIDs) != 1 {
		t.Fatal("superseded attempt retried side effects")
	}
}

func TestFinalizerFencesChangesDuringHandoffAndVerification(t *testing.T) {
	for _, phase := range []string{"handoff", "verification", "expiry"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			s, keyring, a := finalizerStore(t)
			browser := &browserFixture{}
			change := func() {
				if _, err := s.DB().ExecContext(ctx, `UPDATE connections SET generation=generation+1,config_revision=config_revision+1,state='AUTH_REQUIRED' WHERE id=?`, a.ConnectionID); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			if phase == "handoff" {
				browser.afterHandoff = change
			}
			verify := verifierFunc(func(context.Context, string, int64, []byte) error {
				calls++
				if phase == "verification" {
					change()
				}
				if phase == "expiry" {
					if _, err := s.DB().ExecContext(ctx, `UPDATE auth_attempts SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), a.ID); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			f := NewFinalizer(Options{Store: s, Browser: browser, Keyring: keyring, Verifier: verify})
			_, err := f.Complete(ctx, a)
			want := ErrConflict
			if phase == "expiry" {
				want = ErrExpired
			}
			if !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			if phase == "handoff" && calls != 0 {
				t.Fatal("verifier received stale handoff")
			}
			var count int
			if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale verifier committed session: %d %v", count, err)
			}
			if browser.completions != 0 {
				t.Fatal("stale finalizer completed browser")
			}
		})
	}
}

func TestFinalizerSanitizesTransportErrors(t *testing.T) {
	for _, phase := range []string{"handoff", "verification"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			s, keyring, a := finalizerStore(t)
			secret := "bot-token/password/001234/cookie/encrypted-envelope"
			browser := &browserFixture{}
			verify := verifierFunc(func(context.Context, string, int64, []byte) error { return errors.New(secret) })
			want := ErrVerificationPending
			if phase == "handoff" {
				browser.handoffError = errors.New(secret)
				want = ErrHandoff
			}
			_, err := NewFinalizer(Options{Store: s, Browser: browser, Keyring: keyring, Verifier: verify}).Complete(ctx, a)
			if !errors.Is(err, want) || strings.Contains(err.Error(), secret) || errors.Unwrap(err) != nil {
				t.Fatalf("transport error leaked or lost classification: %v", err)
			}
		})
	}
}

type crashAfterCommitStore struct{ *storage.Store }

func (s crashAfterCommitStore) CompleteAuthSession(ctx context.Context, attemptID string, encrypted []byte) (storage.Connection, error) {
	conn, err := s.Store.CompleteAuthSession(ctx, attemptID, encrypted)
	if err == nil {
		panic("simulated process exit after durable commit")
	}
	return conn, err
}

func TestFinalizerAdoptsCommitBeforeBrowserCleanup(t *testing.T) {
	ctx := context.Background()
	s, keyring, a := finalizerStore(t)
	browser := &browserFixture{}
	verifications := 0
	verify := verifierFunc(func(context.Context, string, int64, []byte) error { verifications++; return nil })
	f := NewFinalizer(Options{Store: crashAfterCommitStore{s}, Browser: browser, Keyring: keyring, Verifier: verify})
	crashed := false
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered != "simulated process exit after durable commit" {
					panic(recovered)
				}
				crashed = true
			}
		}()
		_, _ = f.Complete(ctx, a)
	}()
	if !crashed || browser.completions != 0 {
		t.Fatal("did not stop precisely after commit before browser cleanup")
	}
	scheduler := &recoveryScheduler{store: s, available: true}
	// The browser may already be gone after a process restart; retrying cleanup
	// must not turn its 404 into a failed verified authentication.
	browser.completeError = errors.New("browser no longer exists")
	f = NewFinalizer(Options{Store: s, Browser: browser, Scheduler: scheduler})
	conn, err := f.Complete(ctx, a)
	if err != nil || conn.State != "MONITORING" {
		t.Fatalf("could not adopt durable verified commit: %v", err)
	}
	if browser.handoffs != 1 || verifications != 1 {
		t.Fatal("restart replayed handoff or verifier after commit")
	}
	run, err := s.GetRecoveryRunByEvent(ctx, a.ConnectionID, a.Generation, a.ID)
	if err != nil || !reflect.DeepEqual(scheduler.runIDs, []string{run.ID}) {
		t.Fatalf("committed recovery intent was not resumed: %v", err)
	}
}

func TestFinalizerRejectsInvalidAdmissionWithoutBankEffects(t *testing.T) {
	for _, state := range []string{"cancelled", "failed", "expired", "superseded", "missing-keyring", "missing-verifier"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			s, keyring, attempt := finalizerStore(t)
			want := ErrConflict
			switch state {
			case "cancelled", "failed":
				if err := s.FinishAuthAttempt(ctx, attempt.ID, strings.ToUpper(state)); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := s.DB().ExecContext(ctx, `UPDATE auth_attempts SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), attempt.ID); err != nil {
					t.Fatal(err)
				}
				want = ErrExpired
			case "superseded":
				if _, err := s.TransitionConnection(ctx, "pause"); err != nil {
					t.Fatal(err)
				}
			default:
				want = ErrUnavailable
			}
			before, err := s.Connection(ctx)
			if err != nil {
				t.Fatal(err)
			}
			browser := &browserFixture{}
			verifications := 0
			options := Options{Store: s, Browser: browser, Keyring: keyring, Verifier: verifierFunc(func(context.Context, string, int64, []byte) error {
				verifications++
				return nil
			})}
			if state == "missing-keyring" {
				options.Keyring = nil
			}
			if state == "missing-verifier" {
				options.Verifier = nil
			}
			finalizer := NewFinalizer(options)
			for range 2 {
				if _, err := finalizer.Complete(ctx, attempt); !errors.Is(err, want) {
					t.Fatalf("rejected admission returned %v, want %v", err, want)
				}
			}
			after, err := s.Connection(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected admission mutated connection: %+v %v", after, err)
			}
			var sessions, runs int
			if err := s.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM sessions),(SELECT count(*) FROM recovery_runs)`).Scan(&sessions, &runs); err != nil {
				t.Fatal(err)
			}
			if browser.handoffs != 0 || browser.completions != 0 || verifications != 0 || sessions != 0 || runs != 0 {
				t.Fatalf("rejected admission caused side effects: handoffs=%d cleanup=%d verify=%d sessions=%d recovery=%d", browser.handoffs, browser.completions, verifications, sessions, runs)
			}
		})
	}
}
