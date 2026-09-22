package main

import (
	"bytes"
	"context"
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

	t.Run("dbtool -active-auth-count -readonly outputs JSON count", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-active-auth-count")
		cmd.Dir = "."
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("dbtool -active-auth-count failed: %v, stderr: %s", err, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"activeCount":0`) {
			t.Fatalf("expected activeCount:0 in stdout, got: %s", stdout.String())
		}
	})

	t.Run("dbtool -schema-compat -readonly verifies compatibility", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-schema-compat", "-min-version", "10")
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

	t.Run("dbtool -session-check -readonly fails closed when no session exists", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-session-check")
		cmd.Dir = "."
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected dbtool -session-check to fail when no connection/session exists, but it succeeded: %s", stdout.String())
		}
	})

	t.Run("dbtool -session-check -readonly succeeds when durable session exists", func(t *testing.T) {
		ctx := context.Background()
		store, err := storage.Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		conn, err := store.ConfigureConnection(ctx, "***1234")
		if err != nil {
			t.Fatalf("configure connection: %v", err)
		}
		attempt, err := store.StartAuthAttempt(ctx, "owner@example.com", 5*time.Minute)
		if err != nil {
			t.Fatalf("start auth attempt: %v", err)
		}
		if _, err := store.CompleteAuthSession(ctx, attempt.ID, []byte("mock-envelope")); err != nil {
			t.Fatalf("complete auth session: %v", err)
		}
		_ = store.Close()

		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-session-check")
		cmd.Dir = "."
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("dbtool -session-check failed: %v, stderr: %s", err, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"status":"ok"`) || !strings.Contains(stdout.String(), conn.ID) {
			t.Fatalf("expected status:ok and connection_id in stdout, got: %s", stdout.String())
		}
	})
}
