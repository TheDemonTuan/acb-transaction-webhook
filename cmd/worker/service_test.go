package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/lock"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerstate"
)

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestWorkerService_VerifySession_GenerationGuard(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "worker_svc_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Configure initial connection at generation 5
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `UPDATE connections SET generation = 5 WHERE id = ?`, conn.ID)
	if err != nil {
		t.Fatal(err)
	}

	ws := &workerService{
		store: store,
	}

	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	for _, tc := range []struct {
		account    string
		generation int64
		code       string
	}{
		{conn.ID, 0, "VERIFICATION_UNAVAILABLE"},
		{conn.ID, -1, "VERIFICATION_UNAVAILABLE"},
		{conn.ID, 4, "VERIFICATION_SUPERSEDED"},
		{conn.ID, 6, "VERIFICATION_SUPERSEDED"},
		{conn.ID, 5, "VERIFICATION_UNAVAILABLE"},
		{"synthetic-private-account", 5, "VERIFICATION_SUPERSEDED"},
	} {
		output.Reset()
		verified, err := ws.VerifySession(ctx, tc.account, tc.generation, []byte("synthetic-private-password"))
		if len(verified) != 0 {
			t.Fatal("rejected request returned a session")
		}
		if authsession.VerificationCode(err) != tc.code || err.Error() != tc.code {
			t.Fatalf("generation=%d code=%q error=%v", tc.generation, tc.code, err)
		}
		decoder := json.NewDecoder(&output)
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if len(record) != 6 || record["msg"] != "ACB session verification result" || record["generation"] != float64(tc.generation) || record["phase"] != "RESTORE" || record["code"] != tc.code {
			t.Fatalf("unsafe or incorrect verification log fields: %v", record)
		}
		for key := range record {
			switch key {
			case "time", "level", "msg", "generation", "phase", "code":
			default:
				t.Fatalf("unsafe verification field %q", key)
			}
		}
		if decoder.More() {
			t.Fatal("worker guard emitted multiple result events")
		}
	}
}

func TestWorkerService_ScheduleRecovery_ValidatesGenerationAndState(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "worker_recovery_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING', generation=5 WHERE id=?", conn.ID); err != nil {
		t.Fatal(err)
	}
	called := false
	ws := &workerService{store: store, recoveryScheduler: recoverySchedulerFunc(func(context.Context, string, int64, string) error {
		called = true
		return nil
	})}
	if err := ws.ScheduleRecovery(ctx, conn.ID, 4, "auth.verified"); err == nil {
		t.Fatal("expected stale generation error")
	}
	if called {
		t.Fatal("scheduler called for stale generation")
	}
	if err := ws.ScheduleRecovery(ctx, conn.ID, 5, "auth.verified"); err != nil {
		t.Fatalf("valid recovery: %v", err)
	}
	if !called {
		t.Fatal("scheduler not called for valid recovery")
	}
}

type recoverySchedulerFunc func(context.Context, string, int64, string) error

func (f recoverySchedulerFunc) ScheduleRecovery(ctx context.Context, connectionID string, generation int64, eventKey string) error {
	return f(ctx, connectionID, generation, eventKey)
}

func TestWorkerService_NotifyAndWake_Uninitialized(t *testing.T) {
	ctx := context.Background()
	ws := &workerService{}

	if err := ws.NotifySettingsChanged(ctx); err == nil {
		t.Fatal("expected error when bank monitor is nil, got nil")
	}
	if err := ws.WakeDispatcher(ctx); err == nil {
		t.Fatal("expected error when dispatcher is nil, got nil")
	}
	if err := ws.RequestSync(ctx); err == nil {
		t.Fatal("expected error when bank monitor is nil, got nil")
	}
	if err := ws.ScheduleRecovery(ctx, "conn", 1, "auth.verified"); err == nil {
		t.Fatal("expected error when storage is nil, got nil")
	}
	if _, err := ws.CreateHistoryJob(ctx, "2026-09-01", "2026-09-02"); err == nil {
		t.Fatal("expected error when store is nil, got nil")
	}
	if err := ws.CancelHistoryJob(ctx, "job_123"); err == nil {
		t.Fatal("expected error when store is nil, got nil")
	}
}

func TestWorkerService_VerifySession_FailsClosedOnStoreError(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "worker_svc_fail_closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, "UPDATE connections SET generation = 1 WHERE id = ?", conn.ID)
	if err != nil {
		t.Fatal(err)
	}

	var upstreamCalls atomic.Int32
	mockTransport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return nil, errors.New("upstream must not be called on database error")
	})
	acbClient, err := acb.NewClient("https://online.acb.com.vn", mockTransport)
	if err != nil {
		t.Fatal(err)
	}

	ws := &workerService{
		store:                 store,
		verifierClient:        acbClient,
		verifierSessionLoader: monitor.NewSessionLoader(store, nil, nil),
	}

	// Close store to simulate database failure
	store.Close()

	_, err = ws.VerifySession(ctx, conn.ID, 1, []byte("pw"))
	if authsession.VerificationCode(err) != "VERIFICATION_UNAVAILABLE" || err.Error() != "VERIFICATION_UNAVAILABLE" {
		t.Fatalf("expected safe unavailable store failure, got %v", err)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("expected zero upstream calls on store error, got %d", upstreamCalls.Load())
	}
}

func TestWorkerService_CreateAndCancelHistoryJob(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "worker_svc_history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}

	runner := monitor.NewHistoryJobRunner(store, nil, nil, nil)
	ws := &workerService{
		store:         store,
		historyRunner: runner,
	}

	// 1. Connection not monitoring
	if _, err := ws.CreateHistoryJob(ctx, "2026-09-01", "2026-09-10"); err == nil {
		t.Fatal("expected error when connection is not MONITORING, got nil")
	}

	// Activate connection
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING' WHERE id = ?", conn.ID); err != nil {
		t.Fatal(err)
	}

	// 2. Invalid range inputs
	if _, err := ws.CreateHistoryJob(ctx, "bad-date", "2026-09-10"); err == nil {
		t.Fatal("expected error for invalid fromDay, got nil")
	}
	if _, err := ws.CreateHistoryJob(ctx, "2026-09-10", "2026-09-01"); err == nil {
		t.Fatal("expected error when fromDay > toDay, got nil")
	}
	if _, err := ws.CreateHistoryJob(ctx, "2026-08-01", "2026-09-10"); err == nil {
		t.Fatal("expected error when range exceeds 31 days, got nil")
	}

	// 3. Valid job creation
	job, err := ws.CreateHistoryJob(ctx, "2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatalf("CreateHistoryJob failed: %v", err)
	}
	if job.Status != storage.HistoryJobStatusQueued || job.RangeFrom != "2026-09-01" {
		t.Fatalf("unexpected job descriptor: %+v", job)
	}

	// 4. Cancel job
	if err := ws.CancelHistoryJob(ctx, job.ID); err != nil {
		t.Fatalf("CancelHistoryJob failed: %v", err)
	}
	canceledJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetHistorySyncJob: %v", err)
	}
	if canceledJob.Status != storage.HistoryJobStatusCanceled {
		t.Fatalf("expected job status CANCELED, got %s", canceledJob.Status)
	}
}

func TestWorkerShutdownGracefulRequeueAndReleaseLock(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "worker_shutdown_test.db")
	lockPath := filepath.Join(tempDir, "gateway.lock")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING' WHERE id = ?", conn.ID); err != nil {
		t.Fatal(err)
	}

	// 1. Worker acquires singleton flock
	flock, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatalf("acquire singleton flock: %v", err)
	}

	// 2. Second concurrent lock acquisition fails immediately
	if _, err := lock.Acquire(lockPath); err == nil {
		t.Fatal("expected second flock acquisition to fail while first worker holds it")
	}

	// 3. Worker creates a history job and claims it (status RUNNING)
	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	_, claimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !claimed {
		t.Fatal("failed to claim job")
	}

	coordinator := workerstate.NewCoordinator()
	if err := coordinator.SetReady(); err != nil {
		t.Fatal(err)
	}

	// 4. Register stop hook that requeues running jobs on shutdown
	coordinator.RegisterStopHook(func(stopCtx context.Context) error {
		_, err := store.RequeueRunningHistorySyncJobs(stopCtx, "graceful shutdown test")
		return err
	})

	// 5. Worker shuts down: Stop coordinator, then release flock
	if err := coordinator.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := coordinator.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	flock.Close()

	// 6. Verify job was requeued to QUEUED with WORKER_SHUTDOWN error code
	j, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != storage.HistoryJobStatusQueued {
		t.Fatalf("expected requeued job status QUEUED, got %s", j.Status)
	}
	if j.ErrorCode != "WORKER_SHUTDOWN" {
		t.Fatalf("expected error code WORKER_SHUTDOWN, got %s", j.ErrorCode)
	}

	// 7. Next worker can acquire lock immediately and claim the requeued job
	newFlock, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatalf("new worker failed to acquire lock after old worker shutdown: %v", err)
	}
	defer newFlock.Close()

	claimedJob, reclaimed, err := store.ClaimNextHistorySyncJob(ctx, time.Now().UTC())
	if err != nil || !reclaimed {
		t.Fatalf("new worker failed to claim requeued job: %v", err)
	}
	if claimedJob.ID != job.ID {
		t.Fatalf("expected reclaimed job %s, got %s", job.ID, claimedJob.ID)
	}
}

func TestWorkerService_NotificationProviderMetadata(t *testing.T) {
	ctx := context.Background()

	// 1. Unconfigured Bark (barkSender is nil)
	wsUnconf := &workerService{
		barkSender:    nil,
		barkPublicURL: "",
	}
	respUnconf, err := wsUnconf.NotificationProviderMetadata(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(respUnconf.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(respUnconf.Providers))
	}
	for _, p := range respUnconf.Providers {
		if p.ID == "BARK" {
			if p.Configured {
				t.Fatal("expected Bark to be unconfigured")
			}
			if p.Status != "unconfigured" {
				t.Fatalf("expected status unconfigured, got %q", p.Status)
			}
		}
		if p.ID == "WEBHOOK" {
			if !p.Configured {
				t.Fatal("expected Webhook to be configured")
			}
		}
	}

	// 2. Configured Bark
	barkSender := bark.NewSender(bark.Config{
		ServerURL: "http://127.0.0.1:8080",
		PublicURL: "https://bark.example.com",
	}, nil, "")
	wsConf := &workerService{
		barkSender:    barkSender,
		barkPublicURL: "https://bark.example.com",
	}
	respConf, err := wsConf.NotificationProviderMetadata(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var barkFound bool
	for _, p := range respConf.Providers {
		if p.ID == "BARK" {
			barkFound = true
			if !p.Configured {
				t.Fatal("expected Bark to be configured")
			}
			if p.Status != "configured" {
				t.Fatalf("expected status configured, got %q", p.Status)
			}
			if p.PublicURL != "https://bark.example.com" {
				t.Fatalf("expected publicUrl https://bark.example.com, got %q", p.PublicURL)
			}
		}
	}
	if !barkFound {
		t.Fatal("BARK provider not found in response")
	}
}
