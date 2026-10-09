package storage_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestDeploymentControl_Lifecycle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_gate_lifecycle.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Initial gate state must be OPEN
	gate, err := store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate: %v", err)
	}
	if gate.GateState != "OPEN" {
		t.Fatalf("expected initial gate state OPEN, got %s", gate.GateState)
	}
	if !gate.TableExists {
		t.Fatalf("expected tableExists true after migration")
	}

	// Normal mutations must be allowed
	if err := store.CheckMutationAllowed(ctx); err != nil {
		t.Fatalf("check mutation allowed: %v", err)
	}

	// Acquire gate
	acquired, err := store.AcquireMutationGate(ctx, "deploy-worker-1", 5*time.Second, "worker-upgrade")
	if err != nil {
		t.Fatalf("acquire gate: %v", err)
	}
	if acquired.GateState != "LOCKED" || acquired.Owner != "deploy-worker-1" {
		t.Fatalf("unexpected acquired gate: %+v", acquired)
	}
	if acquired.LeaseToken == "" {
		t.Fatalf("expected non-empty lease token")
	}

	// Mutations must now be BLOCKED
	err = store.CheckMutationAllowed(ctx)
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Fatalf("expected ErrMutationGateLocked, got %v", err)
	}

	// Another deployer cannot acquire active lease
	_, err = store.AcquireMutationGate(ctx, "deploy-schema-2", 5*time.Second, "schema-migration")
	if !errors.Is(err, storage.ErrMutationGateHeld) {
		t.Fatalf("expected ErrMutationGateHeld, got %v", err)
	}

	// Renew lease
	if err := store.RenewMutationGate(ctx, "deploy-worker-1", acquired.LeaseToken, 10*time.Second); err != nil {
		t.Fatalf("renew lease: %v", err)
	}

	// Wrong token cannot renew or release
	if err := store.ReleaseMutationGate(ctx, "deploy-worker-1", "wrong-token"); !errors.Is(err, storage.ErrInvalidLeaseToken) {
		t.Fatalf("expected ErrInvalidLeaseToken, got %v", err)
	}

	// Release lease with correct owner and token
	if err := store.ReleaseMutationGate(ctx, "deploy-worker-1", acquired.LeaseToken); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	// Gate is OPEN again
	gate, err = store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate after release: %v", err)
	}
	if gate.GateState != "OPEN" {
		t.Fatalf("expected gate OPEN after release, got %s", gate.GateState)
	}
	if err := store.CheckMutationAllowed(ctx); err != nil {
		t.Fatalf("mutations should be allowed after release: %v", err)
	}
}

func TestDeploymentControl_StaleOwnerRecovery(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_gate_stale.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Owner A acquires with very short lease (50ms)
	gateA, err := store.AcquireMutationGate(ctx, "owner-crash-A", 50*time.Millisecond, "worker-deploy")
	if err != nil {
		t.Fatalf("acquire gate A: %v", err)
	}

	// Wait for lease A to expire
	time.Sleep(100 * time.Millisecond)

	// Gate status should indicate stale
	status, err := store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate status: %v", err)
	}
	if !status.IsStale {
		t.Fatalf("expected gate to be marked stale after expiry")
	}

	// Owner B recovers the stale lease
	gateB, err := store.AcquireMutationGate(ctx, "owner-recovery-B", 2*time.Second, "recovery-deploy")
	if err != nil {
		t.Fatalf("acquire gate B: %v", err)
	}
	if gateB.Owner != "owner-recovery-B" {
		t.Fatalf("expected owner B, got %s", gateB.Owner)
	}
	if gateB.FenceGeneration <= gateA.FenceGeneration {
		t.Fatalf("expected fence generation to increase from %d, got %d", gateA.FenceGeneration, gateB.FenceGeneration)
	}

	_ = store.ReleaseMutationGate(ctx, "owner-recovery-B", gateB.LeaseToken)
}

func TestDeploymentControl_ActiveAuthDoesNotBlockGate(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "active_auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedPayOSCutoverLegacyState(t, store)
	gate, err := store.AcquireMutationGate(ctx, "deployer", time.Minute, "payos-cutover")
	if err != nil {
		t.Fatalf("legacy active auth must not block acquisition: %v", err)
	}
	if gate.GateState != "LOCKED" || gate.LeaseToken == "" || gate.FenceGeneration == 0 {
		t.Fatalf("invalid lease: %+v", gate)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM auth_attempts WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("acquisition must not mutate legacy auth: count=%d err=%v", count, err)
	}
}

func TestDeploymentControl_MutationGuards(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_gate_guards.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Acquire gate to lock mutations
	gate, err := store.AcquireMutationGate(ctx, "deploy-worker", 5*time.Minute, "upgrade")
	if err != nil {
		t.Fatalf("acquire gate: %v", err)
	}

	// CreateEndpoint must fail while locked.
	_, err = store.CreateEndpoint(ctx, "webhook", "https://example.com/wh")
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("CreateEndpoint: expected ErrMutationGateLocked, got %v", err)
	}

	// SaveVoiceSettings must fail while locked.
	_, err = store.SaveVoiceSettings(ctx, storage.VoiceSettings{ProviderMode: "ONLINE_AUTO", EdgeVoice: "vi-VN-HoaiMyNeural"})
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("SaveVoiceSettings: expected ErrMutationGateLocked, got %v", err)
	}

	// CreateBarkChannel must fail while locked.
	_, err = store.CreateBarkChannel(ctx, "bark-chan", "device-key-1", nil)
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("CreateBarkChannel: expected ErrMutationGateLocked, got %v", err)
	}

	// Release gate
	if err := store.ReleaseMutationGate(ctx, "deploy-worker", gate.LeaseToken); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	if _, err := store.CreateEndpoint(ctx, "webhook", "https://example.com/wh"); err != nil {
		t.Fatalf("CreateEndpoint after release failed: %v", err)
	}
}

func TestDeploymentControl_BootstrapTransition(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_bootstrap.db")

	// Open raw store without migrations to simulate pre-v9 database
	rawStore, err := storage.OpenWithOptions(ctx, dbPath, storage.OpenOptions{RunMigrations: false})
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	defer rawStore.Close()

	// Gate status in bootstrap mode
	gate, err := rawStore.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate in bootstrap: %v", err)
	}
	if !gate.Bootstrap || gate.TableExists || gate.GateState != "OPEN" {
		t.Fatalf("unexpected bootstrap gate: %+v", gate)
	}

	// Check mutation allowed should pass in bootstrap mode
	if err := rawStore.CheckMutationAllowed(ctx); err != nil {
		t.Fatalf("mutation allowed in bootstrap: %v", err)
	}

	// Gate acquisition also succeeds in bootstrap mode.
	acq, err := rawStore.AcquireMutationGate(ctx, "bootstrapper", time.Minute, "init")
	if err != nil {
		t.Fatalf("acquire in bootstrap: %v", err)
	}
	if !acq.Bootstrap {
		t.Fatalf("expected bootstrap flag on acquired gate")
	}

	_ = rawStore.Close()

	// Now run full migrations (1 to 9)
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer store.Close()

	// After migration, gate is fully initialized in database
	gate, err = store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate after migration: %v", err)
	}
	if gate.Bootstrap || !gate.TableExists || gate.GateState != "OPEN" {
		t.Fatalf("unexpected post-migration gate: %+v", gate)
	}
}

func TestDeploymentControl_ConcurrentWriteMuSerialization(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_writemu.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				owner := fmt.Sprintf("worker-%d-%d", idx, j)
				gate, err := store.AcquireMutationGate(ctx, owner, 1*time.Minute, "test")
				if err != nil {
					continue
				}
				_ = store.RenewMutationGate(ctx, owner, gate.LeaseToken, 1*time.Minute)
				_ = store.ReleaseMutationGate(ctx, owner, gate.LeaseToken)
			}
		}(i)
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = store.AppendJournalEvent(ctx, "ep1", "test.event", fmt.Sprintf("t-%d-%d", idx, j), []byte(`{}`))
			}
		}(i)
	}

	wg.Wait()
}

func seedPayOSCutoverLegacyState(t *testing.T, store *storage.Store) {
	t.Helper()
	stamp := "2026-01-01T00:00:00Z"
	for i, status := range []string{"STARTING", "IN_PROGRESS", "EXPORTING", "VERIFYING"} {
		connectionID := fmt.Sprintf("legacy-%d", i)
		if _, err := store.DB().Exec(`INSERT INTO connections(id,bank_code,state,generation,config_revision,account_envelope,created_at,updated_at)
			VALUES(?, 'ACB','AUTH_STARTING',7,3,X'010203',?,?)`, connectionID, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO auth_attempts(id,connection_id,generation,status,expires_at,created_at)
			VALUES(?,?,7,?,?,?)`, "attempt-"+status, connectionID, status, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	statements := []string{
		`INSERT INTO auth_attempts(id,connection_id,generation,status,expires_at,created_at,finished_at) VALUES('terminal-verified','legacy-0',6,'VERIFIED','2026-01-01','2026-01-01','2026-01-02'),('terminal-cancelled','legacy-0',5,'CANCELLED','2026-01-01','2026-01-01','2026-01-02')`,
		`INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES('legacy-0',7,X'030405','master-key','2026-01-01')`,
		`INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES('legacy-0',3,X'060708','master-key','2026-01-01')`,
		`INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,credit,description_envelope,parser_version,first_seen_at) VALUES('old-transaction','legacy-0','semantic-key','canonical-hash','2026-01-01','2026-01-01',12345,X'040506','historic','2026-01-01')`,
		`INSERT INTO events(id,transaction_id,event_type,payload,payload_hash,created_at) VALUES('old-event','old-transaction','transaction.created',X'070809','payload-hash','2026-01-01')`,
		`INSERT INTO dedupe_keys(semantic_key,canonical_hash,event_id,created_at) VALUES('semantic-key','canonical-hash','old-event','2026-01-01')`,
		`INSERT INTO transaction_quarantine(id,connection_id,semantic_key,candidate_envelope,reason,created_at) VALUES('old-quarantine','legacy-0','conflict-key',X'010203','conflict','2026-01-01')`,
		`INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at) VALUES('old-endpoint','retained','ACTIVE',1,'2026-01-01','2026-01-01')`,
		`INSERT INTO endpoint_versions(endpoint_id,revision,url,filters_json,created_at) VALUES('old-endpoint',1,'https://example.com','{}','2026-01-01')`,
		`INSERT INTO endpoint_secrets(endpoint_id,key_id,envelope,status,created_at) VALUES('old-endpoint','master-key',X'0A0B0C','ACTIVE','2026-01-01')`,
		`INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,next_attempt_at,created_at,updated_at) VALUES('old-delivery','old-event','old-endpoint',1,'master-key','PENDING','2026-01-01','2026-01-01','2026-01-01')`,
		`INSERT INTO delivery_attempts(id,delivery_id,attempt_number,outcome,created_at) VALUES('old-delivery-attempt','old-delivery',1,'RETRY','2026-01-01')`,
		`INSERT INTO history_sync_jobs(id,connection_id,range_from,range_to,status,generation,created_at,updated_at) VALUES('old-history-job','legacy-0','2026-01-01','2026-01-02','QUEUED',7,'2026-01-01','2026-01-01')`,
		`INSERT INTO history_coverage(id,connection_id,day,status,last_sync_at,rows_seen) VALUES('old-coverage','legacy-0','2026-01-01','COMPLETE','2026-01-01',1)`,
		`INSERT INTO audit_logs(id,action,target,request_id,details_json,created_at) VALUES('old-audit','HISTORIC','legacy-0','old-request','{}','2026-01-01')`,
	}
	for _, statement := range statements {
		if _, err := store.DB().Exec(statement); err != nil {
			t.Fatalf("seed legacy fixture: %v", err)
		}
	}
}

func snapshotCutoverTables(t *testing.T, store *storage.Store) map[string][][]any {
	t.Helper()
	rows, err := store.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	snapshot := make(map[string][][]any, len(tables))
	for _, table := range tables {
		rows, err := store.DB().Query(`SELECT * FROM "` + table + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		snapshot[table] = nil
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				if bytes, ok := value.([]byte); ok {
					values[i] = string(bytes)
				}
			}
			snapshot[table] = append(snapshot[table], values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return snapshot
}

func TestPayOSCutover_RejectsInvalidGateWithoutMutation(t *testing.T) {
	cases := []struct {
		name  string
		token string
		sql   string
	}{
		{"missing-table", "valid-token", `DROP TABLE deployment_control`},
		{"missing-singleton", "valid-token", `DELETE FROM deployment_control`},
		{"open-gate", "valid-token", `UPDATE deployment_control SET gate_state='OPEN'`},
		{"missing-token", "", ""},
		{"wrong-token", "wrong-token", ""},
		{"empty-stored-token", "valid-token", `UPDATE deployment_control SET lease_token=''`},
		{"expired-lease", "valid-token", `UPDATE deployment_control SET lease_expires_at='2000-01-01T00:00:00Z'`},
		{"invalid-expiry", "valid-token", `UPDATE deployment_control SET lease_expires_at='invalid'`},
		{"missing-expiry", "valid-token", `UPDATE deployment_control SET lease_expires_at=NULL`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cutover.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			seedPayOSCutoverLegacyState(t, store)
			if _, err := store.DB().Exec(`UPDATE deployment_control SET gate_state='LOCKED', lease_token='valid-token', lease_expires_at=?`, time.Now().Add(time.Minute).Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			if tc.sql != "" {
				if _, err := store.DB().Exec(tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotCutoverTables(t, store)
			if err := store.PayOSCutover(ctx, tc.token); !errors.Is(err, storage.ErrInvalidLeaseToken) {
				t.Fatalf("expected invalid lease rejection, got %v", err)
			}
			if after := snapshotCutoverTables(t, store); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected cutover mutated database rows")
			}
		})
	}
}

func TestPayOSCutover_PreservesHistoryAndOutbox(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cutover.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedPayOSCutoverLegacyState(t, store)
	gate, err := store.AcquireMutationGate(ctx, "cutover-owner", time.Minute, "payos-cutover")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotCutoverTables(t, store)
	if err := store.PayOSCutover(ctx, gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	after := snapshotCutoverTables(t, store)
	for table, rows := range before {
		if table == "connections" || table == "auth_attempts" || table == "audit_logs" {
			continue
		}
		if !reflect.DeepEqual(rows, after[table]) {
			t.Errorf("cutover changed preserved table %s", table)
		}
	}
	var count int
	assertCount := func(query string, want int) {
		t.Helper()
		if err := store.DB().QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatalf("query %s: count=%d want=%d err=%v", query, count, want, err)
		}
	}
	assertCount(`SELECT count(*) FROM auth_attempts WHERE id LIKE 'attempt-%' AND status='CANCELLED' AND finished_at IS NOT NULL`, 4)
	assertCount(`SELECT count(*) FROM auth_attempts WHERE id LIKE 'terminal-%' AND finished_at='2026-01-02' AND status IN ('VERIFIED','CANCELLED')`, 2)
	assertCount(`SELECT count(*) FROM connections WHERE bank_code='ACB' AND state='PAUSED' AND generation=7 AND config_revision=3 AND account_envelope=X'010203'`, 4)
	assertCount(`SELECT count(*) FROM connections WHERE id='payos-klb' AND bank_code='KienlongBank' AND state='WEBHOOK' AND generation=0`, 1)
	assertCount(`SELECT count(*) FROM audit_logs WHERE action='PAYOS_CUTOVER' AND actor_subject='cutover-owner' AND target='singleton'`, 1)
	assertCount(`SELECT count(*) FROM audit_logs WHERE id='old-audit' AND action='HISTORIC' AND request_id='old-request'`, 1)
}

func TestPayOSCutover_AuditFailureRollsBackState(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedPayOSCutoverLegacyState(t, store)
	gate, err := store.AcquireMutationGate(ctx, "deployer", time.Minute, "cutover")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`CREATE TRIGGER fail_cutover_audit BEFORE INSERT ON audit_logs WHEN NEW.action='PAYOS_CUTOVER' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	before := snapshotCutoverTables(t, store)
	if err := store.PayOSCutover(ctx, gate.LeaseToken); err == nil {
		t.Fatal("expected audit failure")
	}
	if after := snapshotCutoverTables(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("audit failure did not roll back cutover")
	}
}
