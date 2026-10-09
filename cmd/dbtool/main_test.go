package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestDBTool_ReadOnlyProbeFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_dbtool.db")

	// Pre-create database with migrations
	store, err := storage.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("init database: %v", err)
	}
	_ = store.Close()

	t.Run("dbtool -check -readonly succeeds", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-check")
		cmd.Dir = "."
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("dbtool -check failed: %v, stderr: %s", err, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"integrityOk":true`) {
			t.Fatalf("expected integrityOk:true in stdout, got: %s", stdout.String())
		}
	})

	t.Run("dbtool -gate-status -readonly outputs gate JSON", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-gate-status")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("dbtool -gate-status failed: %v, stderr: %s", err, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"gateState":"OPEN"`) || strings.Contains(stdout.String(), "activeAuthCount") {
			t.Fatalf("unexpected gate status JSON: %s", stdout.String())
		}
	})

	t.Run("dbtool -schema-compat -readonly verifies compatibility", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-schema-compat", "-min-version", "13")
		cmd.Dir = "."
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("dbtool -schema-compat failed: %v, stderr: %s", err, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"compatible":true`) {
			t.Fatalf("expected compatible:true in stdout, got: %s", stdout.String())
		}
	})

	t.Run("dbtool -gate-check ignores legacy active auth", func(t *testing.T) {
		store, err := storage.OpenWithOptions(context.Background(), dbPath, storage.OpenOptions{RunMigrations: false})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if _, err := store.DB().Exec(`INSERT INTO connections(id,bank_code,state,created_at,updated_at) VALUES('old-active','ACB','AUTH_STARTING','2026-01-01','2026-01-01')`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO auth_attempts(id,connection_id,generation,status,expires_at,created_at) VALUES('old-active-attempt','old-active',0,'IN_PROGRESS','2099-01-01T00:00:00Z','2026-01-01')`); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-gate-check")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("legacy auth blocked gate-check: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
		}
	})

}

func TestDBTool_RejectsWrongTelegramControlChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	store, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("UPDATE schema_migrations SET checksum='legacy-control' WHERE version=13"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", ".", "-path", path, "-readonly", "-schema-compat", "-min-version", "13")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil {
		t.Fatal("incompatible controller checksum admitted")
	}
	if !strings.Contains(stdout.String(), `"compatible":false`) {
		t.Fatalf("missing rejection: %s", stdout.String())
	}
}

func TestDBTool_PayOSCutover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}
	cases := []struct {
		name    string
		gateSQL string
		args    []string
		wantOK  bool
	}{
		{"valid", "", []string{"-payos-cutover", "-lease-token", "valid-token"}, true},
		{"missing-token", "", []string{"-payos-cutover"}, false},
		{"wrong-token", "", []string{"-payos-cutover", "-lease-token", "wrong-token"}, false},
		{"expired", `UPDATE deployment_control SET lease_expires_at='2000-01-01T00:00:00Z'`, []string{"-payos-cutover", "-lease-token", "valid-token"}, false},
		{"open", `UPDATE deployment_control SET gate_state='OPEN'`, []string{"-payos-cutover", "-lease-token", "valid-token"}, false},
		{"missing-singleton", `DELETE FROM deployment_control`, []string{"-payos-cutover", "-lease-token", "valid-token"}, false},
		{"missing-table", `DROP TABLE deployment_control`, []string{"-payos-cutover", "-lease-token", "valid-token"}, false},
		{"multiple-actions", "", []string{"-payos-cutover", "-lease-token", "valid-token", "-migrate"}, false},
		{"readonly", "", []string{"-payos-cutover", "-lease-token", "valid-token", "-readonly"}, false},
		{"removed-auth-action", "", []string{"-active-auth-count"}, false},
		{"removed-session-action", "", []string{"-session-check"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "cutover.db")
			store, err := storage.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			statements := []string{
				`INSERT INTO connections(id,bank_code,state,generation,created_at,updated_at) VALUES('legacy','ACB','AUTH_STARTING',7,'2026-01-01','2026-01-01')`,
				`INSERT INTO auth_attempts(id,connection_id,generation,status,expires_at,created_at) VALUES('old-auth','legacy',7,'VERIFYING','2026-01-01','2026-01-01')`,
				`INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,credit,parser_version,first_seen_at) VALUES('old-transaction','legacy','semantic-key','hash','2026-01-01','2026-01-01',100,'historic','2026-01-01')`,
				`INSERT INTO events(id,transaction_id,event_type,payload,payload_hash,created_at) VALUES('old-event','old-transaction','transaction.created',X'010203','hash','2026-01-01')`,
				`INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,next_attempt_at,created_at,updated_at) VALUES('old-delivery','old-event','old-endpoint',1,'master-key','PENDING','2026-01-01','2026-01-01','2026-01-01')`,
				`INSERT INTO history_sync_jobs(id,connection_id,range_from,range_to,status,generation,created_at,updated_at) VALUES('old-history','legacy','2026-01-01','2026-01-02','QUEUED',7,'2026-01-01','2026-01-01')`,
				// A mismatched checksum must not invoke auto-migrations.
				`UPDATE schema_migrations SET checksum='cutover-fixture' WHERE version=14`,
			}
			for _, statement := range statements {
				if _, err := store.DB().Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.DB().Exec(`UPDATE deployment_control SET gate_state='LOCKED',owner='deployer',lease_token='valid-token',lease_expires_at=?`, time.Now().Add(5*time.Minute).Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			if tc.gateSQL != "" {
				if _, err := store.DB().Exec(tc.gateSQL); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"run", ".", "-path", path}, tc.args...)
			cmd := exec.Command("go", args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err = cmd.Run()
			if (err == nil) != tc.wantOK {
				t.Fatalf("success=%v want=%v, stdout=%s stderr=%s", err == nil, tc.wantOK, stdout.String(), stderr.String())
			}
			wantAuth, wantConnection, wantAudit := "VERIFYING", "AUTH_STARTING", 0
			if tc.wantOK {
				wantAuth, wantConnection, wantAudit = "CANCELLED", "PAUSED", 1
				if !strings.Contains(stdout.String(), `"action":"PAYOS_CUTOVER"`) {
					t.Fatalf("missing cutover JSON: %s", stdout.String())
				}
			}
			var auth, connection, finishedAt string
			var auditCount int
			if err := store.DB().QueryRow(`SELECT status,COALESCE(finished_at,'') FROM auth_attempts WHERE id='old-auth'`).Scan(&auth, &finishedAt); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT state FROM connections WHERE id='legacy'`).Scan(&connection); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRow(`SELECT count(*) FROM audit_logs WHERE action='PAYOS_CUTOVER'`).Scan(&auditCount); err != nil {
				t.Fatal(err)
			}
			if auth != wantAuth || connection != wantConnection || auditCount != wantAudit || (finishedAt != "") != tc.wantOK {
				t.Fatalf("unexpected state auth=%s connection=%s audit=%d finished=%s", auth, connection, auditCount, finishedAt)
			}
			for _, table := range []string{"transactions", "events", "deliveries", "history_sync_jobs"} {
				var count int
				if err := store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
					t.Fatalf("preserved %s: count=%d err=%v", table, count, err)
				}
			}
			var checksum string
			if err := store.DB().QueryRow(`SELECT checksum FROM schema_migrations WHERE version=14`).Scan(&checksum); err != nil || checksum != "cutover-fixture" {
				t.Fatalf("cutover mutated migration checksum: %s err=%v", checksum, err)
			}
		})
	}
}

func TestDBTool_PayOSCutoverDoesNotBootstrapSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}
	path := filepath.Join(t.TempDir(), "unmigrated.db")
	store, err := storage.OpenWithOptions(context.Background(), path, storage.OpenOptions{RunMigrations: false})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cmd := exec.Command("go", "run", ".", "-path", path, "-payos-cutover", "-lease-token", "token")
	if err := cmd.Run(); err == nil {
		t.Fatal("unmigrated database admitted cutover")
	}
	var tables int
	if err := store.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("cutover created schema: tables=%d err=%v", tables, err)
	}
}

func TestDBTool_PaymentCounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}
	for _, tc := range []struct {
		name       string
		statements []string
		want       storage.PaymentCounts
		wantError  bool
		extraArgs  []string
	}{
		{name: "legacy-unmigrated"},
		{name: "legacy-journal", statements: []string{
			`CREATE TABLE event_journal(seq INTEGER PRIMARY KEY)`,
			`INSERT INTO event_journal(seq) VALUES(2),(9)`,
		}, want: storage.PaymentCounts{JournalSeq: 9}},
		{name: "issued-payments", statements: []string{
			`CREATE TABLE payment_orders(id TEXT PRIMARY KEY)`,
			`CREATE TABLE payment_receipts(reference TEXT PRIMARY KEY)`,
			`CREATE TABLE event_journal(seq INTEGER PRIMARY KEY)`,
			`INSERT INTO payment_orders(id) VALUES('one'),('two'),('three')`,
			`INSERT INTO payment_receipts(reference) VALUES('paid-one'),('paid-two')`,
			`INSERT INTO event_journal(seq) VALUES(1),(8)`,
		}, want: storage.PaymentCounts{Orders: 3, Receipts: 2, JournalSeq: 8}},
		{name: "malformed-journal-fails-closed", statements: []string{
			`CREATE TABLE event_journal(wrong_column INTEGER)`,
		}, wantError: true},
		{name: "multiple-actions", extraArgs: []string{"-migrate"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "counts.db")
			store, err := storage.OpenWithOptions(ctx, path, storage.OpenOptions{RunMigrations: false})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			for _, statement := range tc.statements {
				if _, err := store.DB().Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			var schemaBefore string
			if err := store.DB().QueryRow(`SELECT COALESCE(group_concat(sql,';'),'') FROM (SELECT sql FROM sqlite_master ORDER BY name)`).Scan(&schemaBefore); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"run", ".", "-path", path, "-payment-counts"}, tc.extraArgs...)
			cmd := exec.Command("go", args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v stdout=%s stderr=%s", err, tc.wantError, stdout.String(), stderr.String())
			}
			if !tc.wantError {
				var got storage.PaymentCounts
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatalf("invalid count JSON: %v output=%s", err, stdout.String())
				}
				if got != tc.want {
					t.Fatalf("counts=%+v want=%+v", got, tc.want)
				}
			}
			var schemaAfter string
			if err := store.DB().QueryRow(`SELECT COALESCE(group_concat(sql,';'),'') FROM (SELECT sql FROM sqlite_master ORDER BY name)`).Scan(&schemaAfter); err != nil {
				t.Fatal(err)
			}
			if schemaAfter != schemaBefore {
				t.Fatal("payment count probe changed database schema")
			}
		})
	}
}
