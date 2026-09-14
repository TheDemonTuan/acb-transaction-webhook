package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
		cmd := exec.Command("go", "run", ".", "-path", dbPath, "-readonly", "-schema-compat", "-min-version", "9")
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
}
