package workerrpc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerstate"
)

type quiesceMockHandler struct {
	coordinator *workerstate.Coordinator
	quiesced    bool
	generation  int64
	checkpoint  string
}

func (q *quiesceMockHandler) RequestSync(ctx context.Context) error                          { return nil }
func (q *quiesceMockHandler) CreateHistoryJob(ctx context.Context, from, to string) (storage.HistorySyncJob, error) {
	return storage.HistorySyncJob{ID: "job_1"}, nil
}
func (q *quiesceMockHandler) CancelHistoryJob(ctx context.Context, jobID string) error       { return nil }
func (q *quiesceMockHandler) NotifySettingsChanged(ctx context.Context) error                { return nil }
func (q *quiesceMockHandler) WakeDispatcher(ctx context.Context) error                       { return nil }
func (q *quiesceMockHandler) VerifySession(ctx context.Context, acc string, gen int64, pw []byte) error {
	return nil
}
func (q *quiesceMockHandler) TestNotificationChannel(ctx context.Context, id string) (workerrpc.TestNotificationResponse, error) {
	return workerrpc.TestNotificationResponse{Success: true}, nil
}

func (q *quiesceMockHandler) Quiesce(ctx context.Context) (workerrpc.QuiesceResponse, error) {
	if err := q.coordinator.Quiesce(ctx); err != nil {
		return workerrpc.QuiesceResponse{}, err
	}
	q.quiesced = true
	return workerrpc.QuiesceResponse{
		Status:     "quiesced",
		Quiesced:   true,
		Generation: q.generation,
		Checkpoint: q.checkpoint,
		ScanID:     "scan_123",
	}, nil
}

func (q *quiesceMockHandler) Resume(ctx context.Context) error {
	if err := q.coordinator.Resume(ctx); err != nil {
		return err
	}
	q.quiesced = false
	return nil
}

func (q *quiesceMockHandler) ActivatePaymentWindow(ctx context.Context) error {
	return nil
}

func TestWorkerRPC_QuiesceAndResume(t *testing.T) {
	coordinator := workerstate.NewCoordinator()
	if err := coordinator.SetReady(); err != nil {
		t.Fatalf("set ready: %v", err)
	}

	handler := &quiesceMockHandler{
		coordinator: coordinator,
		generation:  42,
		checkpoint:  "2026-09-14",
	}

	server, err := workerrpc.NewServer(handler, "test-token")
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	server.SetStateProvider(coordinator.State)

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, "test-token")
	ctx := context.Background()

	// 1. Initially worker is ready
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 readyz before quiesce, got %v %d", err, resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 2. Quiesce worker
	qResp, err := client.Quiesce(ctx)
	if err != nil {
		t.Fatalf("quiesce call failed: %v", err)
	}
	if !qResp.Quiesced || qResp.Generation != 42 || qResp.Checkpoint != "2026-09-14" {
		t.Fatalf("unexpected quiesce response: %+v", qResp)
	}

	// 3. While quiesced: upstream requests must be rejected with 503
	_, err = client.CreateHistoryJob(ctx, "2026-09-01", "2026-09-02")
	if err == nil {
		t.Fatalf("expected CreateHistoryJob to fail while quiesced")
	}

	err = client.VerifySession(ctx, "acc1", 42, []byte("pass"))
	if err == nil {
		t.Fatalf("expected VerifySession to fail while quiesced")
	}

	// 4. Healthcheck continues to return 200 OK (worker process is healthy and alive)
	reqHealth, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	respHealth, err := http.DefaultClient.Do(reqHealth)
	if err != nil || respHealth.StatusCode != http.StatusOK {
		t.Fatalf("expected healthz 200 while quiesced, got %v", err)
	}
	_ = respHealth.Body.Close()

	// 5. Resume worker
	if err := client.Resume(ctx); err != nil {
		t.Fatalf("resume call failed: %v", err)
	}

	// 6. After resume: work is allowed again
	job, err := client.CreateHistoryJob(ctx, "2026-09-01", "2026-09-02")
	if err != nil {
		t.Fatalf("CreateHistoryJob failed after resume: %v", err)
	}
	if job.ID != "job_1" {
		t.Fatalf("unexpected job id: %s", job.ID)
	}
}

func TestWorkerRPC_QuiesceCrashAndShutdownPersistence(t *testing.T) {
	coordinator := workerstate.NewCoordinator(
		workerstate.WithDrainTimeout(100*time.Millisecond),
		workerstate.WithShutdownTimeout(100*time.Millisecond),
	)
	_ = coordinator.SetReady()

	var stopHookCalled bool
	coordinator.RegisterStopHook(func(ctx context.Context) error {
		stopHookCalled = true
		return nil
	})

	// Simulate Quiesce -> SIGTERM/crash -> Stop
	ctx := context.Background()
	if err := coordinator.Quiesce(ctx); err != nil {
		t.Fatalf("quiesce: %v", err)
	}

	if err := coordinator.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if !stopHookCalled {
		t.Fatalf("expected stop hook to be called on shutdown after quiesce")
	}
}
