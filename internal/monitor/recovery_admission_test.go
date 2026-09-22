package monitor

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func newRecoveryAdmissionStore(t *testing.T) (*storage.Store, storage.Connection) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "recovery-admission.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING', generation=1 WHERE id=?", conn.ID); err != nil {
		store.Close()
		t.Fatal(err)
	}
	conn.State = "MONITORING"
	conn.Generation = 1
	return store, conn
}

func recoveryRunCount(t *testing.T, store *storage.Store) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM recovery_runs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func completeRecoveryRun(t *testing.T, store *storage.Store, conn storage.Connection, run storage.RecoveryRun) {
	t.Helper()
	ctx := context.Background()
	claimed, err := store.ClaimRecoveryRun(ctx, run.ID, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRecoveryRunProgress(ctx, claimed.ID, conn.ID, conn.Generation, storage.RecoveryRunStatusCompleted, `{}`, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRecoveryAdmissionRunsOnceAcrossMidnight(t *testing.T) {
	ctx := context.Background()
	store, conn := newRecoveryAdmissionStore(t)
	defer store.Close()

	current := time.Date(2026, 9, 22, 23, 59, 59, 0, acb.DefaultLocation)
	mon := New(store, nil, 5*time.Second, 5*time.Second)
	mon.now = func() time.Time { return current }
	mon.admitStartupRecovery(ctx)
	if recoveryRunCount(t, store) != 1 {
		t.Fatal("expected one startup recovery run")
	}

	runs, err := store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected one open startup run, got %d (%v)", len(runs), err)
	}
	completeRecoveryRun(t, store, conn, runs[0])

	current = current.Add(2 * time.Second)
	mon.admitStartupRecovery(ctx)
	if recoveryRunCount(t, store) != 1 {
		t.Fatal("startup admission must not reset at midnight on the same Monitor")
	}
}

func TestStartupRecoverySkipsFullyCoveredPlan(t *testing.T) {
	ctx := context.Background()
	store, conn := newRecoveryAdmissionStore(t)
	defer store.Close()

	now := time.Now().In(acb.DefaultLocation)
	mon := New(store, nil, 5*time.Second, 5*time.Second)
	mon.now = func() time.Time { return now }
	from := now.AddDate(0, 0, -6).Format("2006-01-02")
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, CoverageFrom: from, CoverageTo: from}); err != nil {
		t.Fatal(err)
	}
	dayRows := make(map[string]int, catchUpMaxDays)
	for day := now.AddDate(0, 0, -(catchUpMaxDays - 1)); !day.After(now); day = day.AddDate(0, 0, 1) {
		dayRows[day.Format("2006-01-02")] = 1
	}
	if err := store.RecordCoveragePerDay(ctx, conn.ID, dayRows); err != nil {
		t.Fatal(err)
	}
	mon.admitStartupRecovery(ctx)
	if got := recoveryRunCount(t, store); got != 0 {
		t.Fatalf("fully covered bounded plan must not admit recovery, got %d", got)
	}
}

func TestStartupRecoverySecondBootUsesUniqueWorkerKey(t *testing.T) {
	ctx := context.Background()
	store, conn := newRecoveryAdmissionStore(t)
	defer store.Close()

	first := New(store, nil, 5*time.Second, 5*time.Second)
	first.now = fixedRealtimeTime
	first.admitStartupRecovery(ctx)
	firstRun, err := store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation)
	if err != nil || len(firstRun) != 1 {
		t.Fatalf("expected first startup run, got %d (%v)", len(firstRun), err)
	}
	completeRecoveryRun(t, store, conn, firstRun[0])

	second := New(store, nil, 5*time.Second, 5*time.Second)
	second.now = fixedRealtimeTime
	second.admitStartupRecovery(ctx)
	if got := recoveryRunCount(t, store); got != 2 {
		t.Fatalf("expected one run per worker boot, got %d total runs", got)
	}
	rows, err := store.DB().QueryContext(ctx, "SELECT event_key FROM recovery_runs ORDER BY created_at, id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == keys[1] || !strings.HasPrefix(keys[0], "worker-start:") || !strings.HasPrefix(keys[1], "worker-start:") {
		t.Fatalf("unexpected worker boot keys: %v", keys)
	}
	if strings.Contains(keys[0], "2026-") || strings.Contains(keys[1], "2026-") {
		t.Fatalf("worker boot key must not be date-based: %v", keys)
	}
}

func TestStartupRecoverySkipsAuthAndNonMonitoring(t *testing.T) {
	ctx := context.Background()
	store, conn := newRecoveryAdmissionStore(t)
	defer store.Close()

	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='AUTH_REQUIRED' WHERE id=?", conn.ID); err != nil {
		t.Fatal(err)
	}
	mon := New(store, nil, 5*time.Second, 5*time.Second)
	mon.now = fixedRealtimeTime
	mon.admitStartupRecovery(ctx)
	if got := recoveryRunCount(t, store); got != 0 {
		t.Fatalf("AUTH_REQUIRED must not admit startup recovery, got %d", got)
	}

	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING' WHERE id=?", conn.ID); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO auth_attempts(id, connection_id, generation, owner_subject, status, expires_at, created_at) VALUES(?,?,?,?,?,?,?)`, "auth-active", conn.ID, conn.Generation, "test", "IN_PROGRESS", expires, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	mon = New(store, nil, 5*time.Second, 5*time.Second)
	mon.now = fixedRealtimeTime
	mon.admitStartupRecovery(ctx)
	if got := recoveryRunCount(t, store); got != 0 {
		t.Fatalf("active authentication must block startup recovery, got %d", got)
	}
}

func TestOpenRecoveryReconcilesDurableReason(t *testing.T) {
	ctx := context.Background()
	store, conn := newRecoveryAdmissionStore(t)
	defer store.Close()

	today := fixedRealtimeTime().Format("2006-01-02")
	run, created, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "auth-event", storage.RecoveryRunPlan{
		Reason: "SESSION_AUTHENTICATED", RangeFrom: today, RangeTo: today, NextDay: today,
	})
	if err != nil || !created {
		t.Fatalf("create open recovery run: run=%+v created=%v err=%v", run, created, err)
	}
	probe := &recoveryAdmissionProbeClient{started: make(chan struct{})}
	mon := New(store, probe, 5*time.Second, 5*time.Second)
	mon.now = fixedRealtimeTime
	sched := mon.Scheduler()
	sched.Start(ctx)
	defer sched.Stop()
	mon.reconcileOpenRecovery(ctx)

	select {
	case <-probe.started:
	case <-time.After(2 * time.Second):
		t.Fatal("open recovery was not resumed")
	}
	persisted, err := store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, "auth-event")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Reason != "SESSION_AUTHENTICATED" {
		t.Fatalf("reconciliation changed durable recovery reason to %q", persisted.Reason)
	}
}

func TestOpenRecoveryDoesNotAdmitCompletedAuthRunRepeatedly(t *testing.T) {
	ctx := context.Background()
	store, conn := newRecoveryAdmissionStore(t)
	defer store.Close()

	today := fixedRealtimeTime().Format("2006-01-02")
	if err := store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{today: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE history_coverage SET last_sync_at=? WHERE connection_id=? AND day=?", "2000-01-01T00:00:00Z", conn.ID, today); err != nil {
		t.Fatal(err)
	}
	run, _, err := store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "auth-completed", storage.RecoveryRunPlan{
		Reason: "SESSION_AUTHENTICATED", RangeFrom: today, RangeTo: today, NextDay: today,
	})
	if err != nil {
		t.Fatal(err)
	}
	completeRecoveryRun(t, store, conn, run)

	mon := New(store, nil, 5*time.Second, 5*time.Second)
	mon.now = fixedRealtimeTime
	for i := 0; i < 100; i++ {
		mon.reconcileOpenRecovery(ctx)
	}
	if got := recoveryRunCount(t, store); got != 1 {
		t.Fatalf("periodic reconciliation admitted recovery despite completed auth run: %d total runs", got)
	}
}

type recoveryAdmissionProbeClient struct {
	started   chan struct{}
	startOnce sync.Once
}

func (c *recoveryAdmissionProbeClient) Bootstrap(context.Context) (acb.Response, error) {
	c.startOnce.Do(func() { close(c.started) })
	return acb.Response{StatusCode: 200, Kind: acb.LoginPage}, nil
}

func (c *recoveryAdmissionProbeClient) History(context.Context, string, map[string]string) (acb.Response, error) {
	return acb.Response{StatusCode: 200, Kind: acb.LoginPage}, nil
}
