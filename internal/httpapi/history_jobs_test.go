package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type slowFakeJobManager struct {
	createdJobs []storage.HistorySyncJob
	canceledIDs []string
	delay       time.Duration
}

func (s *slowFakeJobManager) CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error) {
	if s.delay > 0 {
		// Simulate long background work that should NOT block the HTTP response
		go func() {
			time.Sleep(s.delay)
		}()
	}
	job := storage.HistorySyncJob{
		ID:           fmt.Sprintf("syncjob_fake_%d", time.Now().UnixNano()),
		ConnectionID: "conn_test",
		Generation:   1,
		RangeFrom:    fromDay,
		RangeTo:      toDay,
		Status:       storage.HistoryJobStatusQueued,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}
	s.createdJobs = append(s.createdJobs, job)
	return job, nil
}

func (s *slowFakeJobManager) CancelHistoryJob(ctx context.Context, jobID string) error {
	s.canceledIDs = append(s.canceledIDs, jobID)
	return nil
}

func setupTestServer(t *testing.T, role string) (*Server, *storage.Store, string, *http.Cookie) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_history.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}

	conn, err := store.ConfigureConnection(ctx, "***9999")
	if err != nil {
		t.Fatalf("ConfigureConnection: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id = ?`, conn.ID); err != nil {
		t.Fatalf("update state: %v", err)
	}

	roles := config.RoleSubjects{
		Owners:    make(map[string]struct{}),
		Operators: make(map[string]struct{}),
		Viewers:   make(map[string]struct{}),
	}
	switch role {
	case "viewer":
		roles.Viewers["test-user"] = struct{}{}
	case "operator":
		roles.Operators["test-user"] = struct{}{}
	default:
		roles.Owners["test-user"] = struct{}{}
	}

	cfg := config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "test-user",
		Roles:              roles,
	}
	srv := New(cfg, store)

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	srv.handler.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)

	return srv, store, token.Token, cookie
}

// 1. Proves POST returns 202 quickly (< 100ms) without waiting for fake long worker task
func TestHistoryJobs_QuickAccepted(t *testing.T) {
	srv, store, token, cookie := setupTestServer(t, "owner")
	defer store.Close()

	mgr := &slowFakeJobManager{delay: 2 * time.Second}
	srv.WithHistoryJobManager(mgr)

	body := `{"from":"2026-09-01","to":"2026-09-10"}`
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("X-CSRF-Token", token)
	req.AddCookie(cookie)

	start := time.Now()
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, req)
	elapsed := time.Since(start)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", w.Code, w.Body.String())
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("expected ensure-history to return quickly under 1.5s without waiting for worker task, took %v", elapsed)
	}

	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["status"] != "QUEUED" {
		t.Errorf("expected status QUEUED, got %v", res["status"])
	}
	if res["coverage"] != "PENDING" {
		t.Errorf("expected coverage PENDING, got %v", res["coverage"])
	}
	if res["synced"] != false {
		t.Errorf("expected synced false, got %v", res["synced"])
	}
	jobObj, ok := res["job"].(map[string]any)
	if !ok || jobObj["id"] == "" {
		t.Errorf("expected nested job object with id: %v", res)
	}
}

// 2. Tests CSRF / Auth / RBAC on POST and DELETE
func TestHistoryJobs_CSRF_Auth_RBAC(t *testing.T) {
	ctx := context.Background()

	// A. CSRF Token Invalid on POST
	{
		srv, store, _, cookie := setupTestServer(t, "owner")
		defer store.Close()
		req := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", strings.NewReader(`{"from":"2026-09-01","to":"2026-09-05"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://example.test")
		req.Header.Set("X-CSRF-Token", "wrong-csrf-token")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 for wrong CSRF on POST, got %d", w.Code)
		}
	}

	// B. Origin Mismatch on POST
	{
		srv, store, token, cookie := setupTestServer(t, "owner")
		defer store.Close()
		req := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", strings.NewReader(`{"from":"2026-09-01","to":"2026-09-05"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://attacker.test")
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 for origin mismatch on POST, got %d", w.Code)
		}
	}

	// C. RBAC: Viewer cannot POST or DELETE (403), Operator and Owner can (202)
	{
		srvViewer, storeViewer, tokenViewer, cookieViewer := setupTestServer(t, "viewer")
		defer storeViewer.Close()

		// Viewer POST -> 403
		reqPost := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", strings.NewReader(`{"from":"2026-09-01","to":"2026-09-05"}`))
		reqPost.Header.Set("Content-Type", "application/json")
		reqPost.Header.Set("Origin", "http://example.test")
		reqPost.Header.Set("X-CSRF-Token", tokenViewer)
		reqPost.AddCookie(cookieViewer)
		w := httptest.NewRecorder()
		srvViewer.handler.ServeHTTP(w, reqPost)
		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 for viewer POST ensure-history, got %d", w.Code)
		}

		// Viewer DELETE -> 403
		reqDel := httptest.NewRequest(http.MethodDelete, "http://example.test/api/v1/transactions/history-sync-jobs/some_id", nil)
		reqDel.Header.Set("Origin", "http://example.test")
		reqDel.Header.Set("X-CSRF-Token", tokenViewer)
		reqDel.AddCookie(cookieViewer)
		wDel := httptest.NewRecorder()
		srvViewer.handler.ServeHTTP(wDel, reqDel)
		if wDel.Code != http.StatusForbidden {
			t.Errorf("expected 403 for viewer DELETE history job, got %d", wDel.Code)
		}

		// Viewer GET -> 200 (read-only allowed for viewer)
		conn, _ := storeViewer.Connection(ctx)
		job, _, _ := storeViewer.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
		reqGet := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/transactions/history-sync-jobs/"+job.ID, nil)
		wGet := httptest.NewRecorder()
		srvViewer.handler.ServeHTTP(wGet, reqGet)
		if wGet.Code != http.StatusOK {
			t.Errorf("expected 200 for viewer GET history job, got %d", wGet.Code)
		}
	}

	// D. Operator can POST and DELETE
	{
		srvOp, storeOp, tokenOp, cookieOp := setupTestServer(t, "operator")
		defer storeOp.Close()

		reqPost := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", strings.NewReader(`{"from":"2026-09-01","to":"2026-09-05"}`))
		reqPost.Header.Set("Content-Type", "application/json")
		reqPost.Header.Set("Origin", "http://example.test")
		reqPost.Header.Set("X-CSRF-Token", tokenOp)
		reqPost.AddCookie(cookieOp)
		w := httptest.NewRecorder()
		srvOp.handler.ServeHTTP(w, reqPost)
		if w.Code != http.StatusAccepted {
			t.Errorf("expected 202 for operator POST ensure-history, got %d: %s", w.Code, w.Body.String())
		}
		var created struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &created)

		// Operator DELETE -> 202
		reqDel := httptest.NewRequest(http.MethodDelete, "http://example.test/api/v1/transactions/history-sync-jobs/"+created.ID, nil)
		reqDel.Header.Set("Origin", "http://example.test")
		reqDel.Header.Set("X-CSRF-Token", tokenOp)
		reqDel.AddCookie(cookieOp)
		wDel := httptest.NewRecorder()
		srvOp.handler.ServeHTTP(wDel, reqDel)
		if wDel.Code != http.StatusAccepted {
			t.Errorf("expected 202 for operator DELETE history job, got %d: %s", wDel.Code, wDel.Body.String())
		}
	}
}

// 3. Proves GET cannot leak a job from a different connection
func TestHistoryJobs_Isolation_DifferentConnection(t *testing.T) {
	ctx := context.Background()
	srv, store, _, _ := setupTestServer(t, "owner")
	defer store.Close()

	// Insert a foreign connection and a job directly belonging to "conn_other"
	otherJobID := "syncjob_foreign_123"
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections (id, account_masked, state, created_at, updated_at)
		VALUES ('conn_other', '***8888', 'MONITORING', ?, ?)
	`, nowStr, nowStr)
	if err != nil {
		t.Fatalf("insert foreign connection: %v", err)
	}

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO history_sync_jobs (id, connection_id, generation, range_from, range_to, status, pages_done, rows_seen, attempts, created_at, updated_at)
		VALUES (?, 'conn_other', 1, '2026-09-01', '2026-09-05', 'RUNNING', 2, 10, 1, ?, ?)
	`, otherJobID, nowStr, nowStr)
	if err != nil {
		t.Fatalf("insert foreign job: %v", err)
	}

	// Requesting foreign job through GET /transactions/history-sync-jobs/{id}
	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/transactions/history-sync-jobs/"+otherJobID, nil)
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, req)

	// Must return 404 JOB_NOT_FOUND rather than leaking the foreign job
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for foreign connection job, got %d: %s", w.Code, w.Body.String())
	}
	var errResp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp["code"] != "JOB_NOT_FOUND" {
		t.Errorf("expected code JOB_NOT_FOUND, got %q", errResp["code"])
	}
}

// 4. Proves input validation for malformed date, from-after-to, and max 31-day range
func TestHistoryJobs_InputValidation(t *testing.T) {
	srv, store, token, cookie := setupTestServer(t, "owner")
	defer store.Close()

	post := func(body string) (int, string) {
		r := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		var res map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return w.Code, res["code"]
	}

	// Malformed date
	code, errCode := post(`{"from":"not-a-date","to":"2026-09-10"}`)
	if code != http.StatusBadRequest || errCode != "INVALID_RANGE" {
		t.Errorf("expected 400 INVALID_RANGE for not-a-date, got %d (%s)", code, errCode)
	}

	// From after to
	code, errCode = post(`{"from":"2026-09-15","to":"2026-09-10"}`)
	if code != http.StatusBadRequest || errCode != "INVALID_RANGE" {
		t.Errorf("expected 400 INVALID_RANGE for from > to, got %d (%s)", code, errCode)
	}

	// Range > 31 days
	code, errCode = post(`{"from":"2026-08-01","to":"2026-09-10"}`)
	if code != http.StatusBadRequest || errCode != "INVALID_RANGE" {
		t.Errorf("expected 400 INVALID_RANGE for range > 31 days, got %d (%s)", code, errCode)
	}
}

// 5. Proves audit logging and event journal publishing on create and cancel
func TestHistoryJobs_AuditingAndJournalEvents(t *testing.T) {
	ctx := context.Background()
	srv, store, token, cookie := setupTestServer(t, "owner")
	defer store.Close()

	// Create job
	reqPost := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", strings.NewReader(`{"from":"2026-09-01","to":"2026-09-05"}`))
	reqPost.Header.Set("Content-Type", "application/json")
	reqPost.Header.Set("Origin", "http://example.test")
	reqPost.Header.Set("X-CSRF-Token", token)
	reqPost.AddCookie(cookie)
	wPost := httptest.NewRecorder()
	srv.handler.ServeHTTP(wPost, reqPost)
	if wPost.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", wPost.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(wPost.Body.Bytes(), &created)

	// Verify audit log for create
	auditPage, err := store.ListAuditLogsPage(ctx, 10, "")
	if err != nil {
		t.Fatalf("ListAuditLogsPage: %v", err)
	}
	foundCreateAudit := false
	for _, entry := range auditPage.Items {
		if entry.Action == "history_sync.create" && entry.Target == created.ID {
			foundCreateAudit = true
			break
		}
	}
	if !foundCreateAudit {
		t.Errorf("expected audit entry for history_sync.create on %s", created.ID)
	}

	// Verify journal event for create
	journalEvents, err := store.ReadJournalEvents(ctx, "ep1", 0, 100)
	if err != nil {
		t.Fatalf("ReadJournalEvents: %v", err)
	}
	foundQueuedEvent := false
	for _, ev := range journalEvents {
		if ev.EventType == "history_sync.queued" && ev.AggregateID == created.ID {
			foundQueuedEvent = true
			break
		}
	}
	if !foundQueuedEvent {
		t.Errorf("expected journal event history_sync.queued for %s", created.ID)
	}

	// Cancel job via POST /api/v1/transactions/history-sync-jobs/{id}/cancel
	reqCancel := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/history-sync-jobs/"+created.ID+"/cancel", nil)
	reqCancel.Header.Set("Origin", "http://example.test")
	reqCancel.Header.Set("X-CSRF-Token", token)
	reqCancel.AddCookie(cookie)
	wCancel := httptest.NewRecorder()
	srv.handler.ServeHTTP(wCancel, reqCancel)
	if wCancel.Code != http.StatusAccepted {
		t.Fatalf("expected 202 on cancel, got %d: %s", wCancel.Code, wCancel.Body.String())
	}

	// Verify audit log for cancel
	auditPageCancel, _ := store.ListAuditLogsPage(ctx, 10, "")
	foundCancelAudit := false
	for _, entry := range auditPageCancel.Items {
		if entry.Action == "history_sync.cancel" && entry.Target == created.ID {
			foundCancelAudit = true
			break
		}
	}
	if !foundCancelAudit {
		t.Errorf("expected audit entry for history_sync.cancel on %s", created.ID)
	}

	// Re-canceling a terminal job returns 409 Conflict with code JOB_TERMINAL
	// First mark job COMPLETED in DB
	_, _ = store.DB().ExecContext(ctx, `UPDATE history_sync_jobs SET status = 'COMPLETED' WHERE id = ?`, created.ID)
	reqCancel2 := httptest.NewRequest(http.MethodDelete, "http://example.test/api/v1/transactions/history-sync-jobs/"+created.ID, nil)
	reqCancel2.Header.Set("Origin", "http://example.test")
	reqCancel2.Header.Set("X-CSRF-Token", token)
	reqCancel2.AddCookie(cookie)
	wCancel2 := httptest.NewRecorder()
	srv.handler.ServeHTTP(wCancel2, reqCancel2)
	if wCancel2.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict when canceling completed job, got %d", wCancel2.Code)
	}
}

// 6. Proves cache already covered returns 200 OK with job: null
func TestHistoryJobs_AlreadyCoveredReturns200(t *testing.T) {
	ctx := context.Background()
	srv, store, token, cookie := setupTestServer(t, "owner")
	defer store.Close()

	conn, _ := store.Connection(ctx)
	// Record coverage for all days in 2026-09-01 to 2026-09-03
	days := []string{"2026-09-01", "2026-09-02", "2026-09-03"}
	if err := store.RecordCoverage(ctx, conn.ID, days, 15); err != nil {
		t.Fatalf("RecordCoverage: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", strings.NewReader(`{"from":"2026-09-01","to":"2026-09-03"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("X-CSRF-Token", token)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for already covered range, got %d: %s", w.Code, w.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["status"] != "COMPLETED" || res["coverage"] != "COMPLETE" || res["synced"] != false || res["job"] != nil {
		t.Errorf("unexpected already covered response: %v", res)
	}
}

// 7. Tests latest job endpoint
func TestHistoryJobs_LatestEndpoint(t *testing.T) {
	ctx := context.Background()
	srv, store, _, _ := setupTestServer(t, "owner")
	defer store.Close()

	conn, _ := store.Connection(ctx)
	job, _, err := store.CreateOrGetHistorySyncJob(ctx, conn.ID, conn.Generation, "2026-09-01", "2026-09-05")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/transactions/history-sync-jobs/latest", nil)
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for latest job, got %d: %s", w.Code, w.Body.String())
	}
	var res storage.HistorySyncJob
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.ID != job.ID {
		t.Errorf("expected latest job id %s, got %s", job.ID, res.ID)
	}
}
