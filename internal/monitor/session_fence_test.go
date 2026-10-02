package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func sessionFenceFixture(t *testing.T) (*storage.Store, storage.Connection, *security.Keyring, []byte) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "fence.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := security.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := authbrowser.EncodeHandoff(authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/AccountSummary", Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: acb.OfficialHost, Path: "/", Secure: true}}}, []byte("synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := keyring.Encrypt([]byte(handoff), security.SessionAAD(conn.ID, conn.Generation))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id=?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,?,?,?,?,?)`, conn.ID, conn.Generation, encoded, envelope.KeyID, now, now); err != nil {
		t.Fatal(err)
	}
	conn.State = "MONITORING"
	return store, conn, keyring, encoded
}

func installLogoutFence(t *testing.T, store *storage.Store, conn storage.Connection) int64 {
	t.Helper()
	generation := conn.Generation + 1
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`UPDATE connections SET generation=?,state='AUTH_REQUIRED' WHERE id=?`, generation, conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM sessions WHERE connection_id=?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO acb_logout_jobs(id,connection_id,old_generation,fenced_generation,state,created_at,updated_at) VALUES('logout-fence',?,?,?,'CLEARING',?,?)`, conn.ID, conn.Generation, generation, now, now); err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestSessionLoaderCacheHitAndEnvelopeRejectLogoutFence(t *testing.T) {
	store, conn, keyring, encoded := sessionFenceFixture(t)
	restorer := &recordingRestorer{}
	loader := NewSessionLoader(store, keyring, restorer)
	ctx := context.Background()
	if err := loader.Restore(ctx, conn.ID, conn.Generation); err != nil {
		t.Fatal(err)
	}
	generation := installLogoutFence(t, store, conn)
	if err := loader.Restore(ctx, conn.ID, conn.Generation); !errors.Is(err, storage.ErrGenerationFenceMismatch) {
		t.Fatalf("cached stale session restored: %v", err)
	}
	if err := loader.RestoreEnvelope(ctx, conn.ID, conn.Generation, encoded); !errors.Is(err, storage.ErrGenerationFenceMismatch) {
		t.Fatalf("stale envelope restored: %v", err)
	}
	if err := loader.InvalidateSession(ctx, conn.ID, generation); err != nil {
		t.Fatal(err)
	}
	if err := loader.InvalidateSession(ctx, conn.ID, generation); err != nil {
		t.Fatal(err)
	}
	if len(restorer.handoff.Cookies) != 0 || loader.loadedID != "" {
		t.Fatal("local session survived invalidation")
	}
	if err := loader.InvalidateSession(ctx, conn.ID, generation-1); err == nil {
		t.Fatal("stale invalidation accepted")
	}
}

func TestSessionLoaderRestoreStateBoundaries(t *testing.T) {
	for _, state := range []string{"AUTH_STARTING", "AUTH_REQUIRED", "MONITORING"} {
		t.Run(state, func(t *testing.T) {
			store, conn, keyring, encoded := sessionFenceFixture(t)
			if _, err := store.DB().Exec(`UPDATE connections SET state=?`, state); err != nil {
				t.Fatal(err)
			}
			loader := NewSessionLoader(store, keyring, &recordingRestorer{})
			err := loader.Restore(context.Background(), conn.ID, conn.Generation)
			if (err == nil) != (state == "MONITORING") {
				t.Fatalf("runtime state=%s error=%v", state, err)
			}
			err = loader.RestoreEnvelope(context.Background(), conn.ID, conn.Generation, encoded)
			if (err == nil) != (state == "MONITORING" || state == "AUTH_STARTING") {
				t.Fatalf("verifier state=%s error=%v", state, err)
			}
		})
	}
}

func TestSessionInvalidationWaitsForRequestAndBlocksContinuation(t *testing.T) {
	store, conn, keyring, _ := sessionFenceFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client, err := acb.NewClient("https://online.acb.com.vn", &verifierMockTransport{roundTripFn: func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	loader := NewSessionLoader(store, keyring, client)
	otherLoader := NewSessionLoader(store, keyring, client)
	if err := loader.Restore(context.Background(), conn.ID, conn.Generation); err != nil {
		t.Fatal(err)
	}
	mon := New(store, client, time.Second, time.Second).WithSessionLoader(loader)
	finished := make(chan error, 1)
	go func() {
		_, err := mon.sessionRequest(context.Background(), conn.ID, conn.Generation, func() (acb.Response, error) { return client.Bootstrap(context.Background()) })
		finished <- err
	}()
	<-entered
	generation := installLogoutFence(t, store, conn)
	cleared := make(chan error, 1)
	go func() { cleared <- otherLoader.InvalidateSession(context.Background(), conn.ID, generation) }()
	select {
	case err := <-cleared:
		t.Fatalf("acknowledged before request completed: %v", err)
	default:
	}
	close(release)
	if err := <-finished; !errors.Is(err, storage.ErrGenerationFenceMismatch) {
		t.Fatalf("in-flight stale result accepted: %v", err)
	}
	if err := <-cleared; err != nil {
		t.Fatal(err)
	}
	// A retained pagination token must never initiate another request after clear.
	runner := NewHistoryJobRunner(store, client, nil, loader)
	_, err = runner.sessionRequest(context.Background(), conn.ID, conn.Generation, func() (acb.Response, error) {
		return client.History(context.Background(), "/acbib/Request", map[string]string{"dse_sessionId": "retained", "dse_processorState": "retained", "dse_nextEventName": "next"})
	})
	if !errors.Is(err, storage.ErrGenerationFenceMismatch) || calls.Load() != 1 {
		t.Fatalf("stale continuation calls=%d err=%v", calls.Load(), err)
	}
	if _, err := client.SnapshotSession(); !errors.Is(err, acb.ErrAuthenticatedFormStateUnavailable) {
		t.Fatalf("snapshot survived clear: %v", err)
	}
}

func TestSessionVerifierTimeoutThenLogoutBeforeQueuedDispatch(t *testing.T) {
	store, conn, keyring, encoded := sessionFenceFixture(t)
	var calls atomic.Int32
	client, err := acb.NewClient("https://online.acb.com.vn", &verifierMockTransport{roundTripFn: func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected upstream")
	}})
	if err != nil {
		t.Fatal(err)
	}
	loader := NewSessionLoader(store, keyring, client)
	sched := scheduler.New(nil)
	schedulerCtx, stop := context.WithCancel(context.Background())
	defer stop()
	entered, release := make(chan struct{}), make(chan struct{})
	blocker := &monitorMockTask{id: "logout-blocker", priority: PriorityKeepalive, stepFn: func(context.Context) (TaskStepResult, error) {
		close(entered)
		<-release
		return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
	}}
	sched.Start(schedulerCtx)
	defer sched.Stop()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if err := sched.Enqueue(blocker); err != nil {
		t.Fatal(err)
	}
	<-entered
	verifier := NewSessionVerifier(loader, client, sched)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := verifier.VerifySession(ctx, conn.ID, conn.Generation, encoded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout=%v", err)
	}
	generation := installLogoutFence(t, store, conn)
	if err := verifier.InvalidateSession(context.Background(), conn.ID, generation); err != nil {
		t.Fatal(err)
	}
	// Also exercise a queued request whose RPC context has not expired: the fence,
	// not just scheduler cancellation, must prevent resurrection on dispatch.
	queuedDone := make(chan error, 1)
	go func() { queuedDone <- verifier.VerifySession(context.Background(), conn.ID, conn.Generation, encoded) }()
	close(release)
	select {
	case err := <-queuedDone:
		if !errors.Is(err, storage.ErrGenerationFenceMismatch) {
			t.Fatalf("queued verify=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued verify did not finish")
	}
	if calls.Load() != 0 || loader.loadedID != "" {
		t.Fatalf("session resurrected: calls=%d loaded=%s", calls.Load(), loader.loadedID)
	}
}

func TestRealtimeTaskLogoutBetweenPagesDropsContinuation(t *testing.T) {
	store, conn, _, _ := sessionFenceFixture(t)
	client := &multiPageMockClient{maxPages: 10}
	mon := New(store, client, time.Second, time.Second)
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	result, err := task.Step(context.Background())
	if err != nil || result.Done {
		t.Fatalf("first quantum=%+v err=%v", result, err)
	}
	before := client.pagesReturned.Load()
	if task.nextFields == nil {
		t.Fatal("fixture has no retained continuation")
	}
	installLogoutFence(t, store, conn)
	_, _ = task.Step(context.Background())
	if client.pagesReturned.Load() != before || task.nextFields != nil || task.nextAction != "" {
		t.Fatalf("stale pages resumed: before=%d after=%d", before, client.pagesReturned.Load())
	}
}

func TestCatchupAndHistoryRetainedTokensRespectLogout(t *testing.T) {
	store, conn, _, _ := sessionFenceFixture(t)
	client := &multiPageMockClient{maxPages: 10}
	mon := New(store, client, time.Second, time.Second)
	runner := NewHistoryJobRunner(store, client, nil, nil)
	catchup := newCatchUpTask(mon, conn.ID, conn.Generation, "test", "")
	catchup.nextAction, catchup.nextFields = "/history", map[string]string{"dse_sessionId": "retained", "dse_processorState": "retained"}
	history := &HistoryJobTask{runner: runner, job: storage.HistorySyncJob{ConnectionID: conn.ID, Generation: conn.Generation}, nextAction: "/history", nextFields: map[string]string{"dse_sessionId": "retained", "dse_processorState": "retained"}}
	installLogoutFence(t, store, conn)
	_, err := catchup.sessionRequest(context.Background(), func() (acb.Response, error) {
		return client.History(context.Background(), catchup.nextAction, catchup.nextFields)
	})
	if !errors.Is(err, storage.ErrGenerationFenceMismatch) || catchup.nextFields != nil {
		t.Fatalf("catchup reused tokens: %v", err)
	}
	_, err = history.sessionRequest(context.Background(), func() (acb.Response, error) {
		return client.History(context.Background(), history.nextAction, history.nextFields)
	})
	if !errors.Is(err, storage.ErrGenerationFenceMismatch) || history.nextFields != nil || client.pagesReturned.Load() != 0 {
		t.Fatalf("history reused tokens: calls=%d error=%v", client.pagesReturned.Load(), err)
	}
}
