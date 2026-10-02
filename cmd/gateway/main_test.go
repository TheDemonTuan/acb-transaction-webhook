package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestParseGatewayFlags_MigrateOnlyFails(t *testing.T) {
	var buf bytes.Buffer
	_, err := parseGatewayFlags([]string{"--migrate-only"}, &buf)
	if err == nil {
		t.Fatal("expected error when --migrate-only is provided, got nil")
	}
	if !strings.Contains(err.Error(), "dbtool --migrate") {
		t.Errorf("expected actionable error mentioning 'dbtool --migrate', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "--migrate-only") {
		t.Errorf("expected error mentioning '--migrate-only', got %q", err.Error())
	}
}

func TestParseGatewayFlags_ValidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want gatewayFlags
	}{
		{
			name: "empty args",
			args: []string{},
			want: gatewayFlags{},
		},
		{
			name: "healthcheck",
			args: []string{"--healthcheck"},
			want: gatewayFlags{healthcheck: true},
		},
		{
			name: "deploycheck",
			args: []string{"--deploycheck"},
			want: gatewayFlags{deploycheck: true},
		},
		{
			name: "check",
			args: []string{"--check"},
			want: gatewayFlags{checkIntegrity: true},
		},
		{
			name: "backup-to",
			args: []string{"--backup-to", "/tmp/backup.db"},
			want: gatewayFlags{backupTo: "/tmp/backup.db"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, err := parseGatewayFlags(tt.args, io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if flags.healthcheck != tt.want.healthcheck {
				t.Errorf("healthcheck: got %v, want %v", flags.healthcheck, tt.want.healthcheck)
			}
			if flags.checkIntegrity != tt.want.checkIntegrity {
				t.Errorf("checkIntegrity: got %v, want %v", flags.checkIntegrity, tt.want.checkIntegrity)
			}
			if flags.backupTo != tt.want.backupTo {
				t.Errorf("backupTo: got %q, want %q", flags.backupTo, tt.want.backupTo)
			}
		})
	}
}

func TestGatewayMigrateOnly_Subprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	cmd := exec.Command("go", "run", ".", "--migrate-only")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "LISTEN_ADDR=127.0.0.1:0")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected gateway --migrate-only subprocess to exit with error, got 0")
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "dbtool --migrate") {
		t.Errorf("expected stderr to contain 'dbtool --migrate', got: %s", stderrStr)
	}
}

func TestGatewayRoleValidation_Subprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	tests := []struct {
		name       string
		env        []string
		wantStderr string
	}{
		{
			name: "rejects worker role on gateway",
			env: []string{
				"APP_ENV=development",
				"RUNTIME_ROLE=worker",
			},
			wantStderr: "unsupported runtime role for gateway",
		},
		{
			name: "production rejects monolith-dev role",
			env: []string{
				"APP_ENV=production",
				"RUNTIME_ROLE=monolith-dev",
			},
			wantStderr: "monolith-dev role is forbidden in production",
		},
		{
			name: "production gateway rejects missing worker RPC URL",
			env: []string{
				"APP_ENV=production",
				"RUNTIME_ROLE=gateway",
				"APP_MASTER_KEY_FILE=/dev/null",
				"OWNER_SUBJECTS=owner@example.com",
				"CF_ACCESS_ISSUER=https://test.cloudflareaccess.com",
				"CF_ACCESS_AUDIENCE=aud123",
				"CF_ACCESS_JWKS_URL=https://test.cloudflareaccess.com/certs",
				"TTS_INTERNAL_TOKEN=token",
				"WORKER_RPC_URL=",
			},
			wantStderr: "WORKER_RPC_URL is required for gateway in production",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("go", "run", ".", "--healthcheck")
			// We run without healthcheck flag to exercise main role validation
			cmd = exec.Command("go", "run", ".")
			cmd.Dir = "."
			// Clean env plus test env
			cmd.Env = append(os.Environ(), tc.env...)

			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			if err == nil {
				t.Fatalf("expected gateway startup to fail for %s, but it exited 0", tc.name)
			}
			out := stderr.String() + stdout.String()
			if !strings.Contains(out, tc.wantStderr) {
				t.Fatalf("expected output to contain %q, got %q", tc.wantStderr, out)
			}
		})
	}
}

func TestTwoGatewaysNoSingletonMaintenance(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "shared_gateway.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	// Seed 48-hour-old journal event
	oldTime := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO event_journal(epoch, event_type, aggregate_id, payload_json, created_at)
		VALUES('ep1', 'old.event', 'old_agg_1', '{}', ?)
	`, oldTime)
	if err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	// Gateway A (Blue)
	cfgA := config.Config{
		Production:         false,
		Slot:               "blue",
		DevelopmentSubject: "dev@example.com",
	}
	gwA := httpapi.New(cfgA, store).WithEventHub(eventhub.New())

	// Gateway B (Green)
	cfgB := config.Config{
		Production:         false,
		Slot:               "green",
		DevelopmentSubject: "dev@example.com",
	}
	gwB := httpapi.New(cfgB, store).WithEventHub(eventhub.New())

	// Run both gateways concurrently for 100ms
	srvA := httptest.NewServer(gwA.Handler())
	defer srvA.Close()
	srvB := httptest.NewServer(gwB.Handler())
	defer srvB.Close()

	time.Sleep(100 * time.Millisecond)

	// Verify old journal event was NOT deleted by either gateway
	events, err := store.ReadJournalEvents(ctx, "ep1", 0, 10)
	if err != nil {
		t.Fatalf("ReadJournalEvents: %v", err)
	}
	if len(events) != 1 || events[0].AggregateID != "old_agg_1" {
		t.Fatalf("expected old journal event to remain untouched by gateways, got %v", events)
	}
}

func TestGatewayPollNotifier_RepeatSuccessfulEmptyPolls(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "gateway_poll_test.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	hub := eventhub.New()
	_, ch, cancel := hub.Subscribe()
	defer cancel()

	notifier := newGatewayPollNotifier(store, hub, nil)

	// Fire repeated SUCCEEDED polls with 0 items
	for i := 1; i <= 3; i++ {
		poll := storage.PollRun{
			ID:        fmt.Sprintf("gw_poll_%03d", i),
			Status:    "SUCCEEDED",
			StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			RowsSeen:  0,
		}
		notifier(poll, 0)
	}

	// Verify journal has all 3 poll.completed events
	events, err := store.ReadJournalEvents(ctx, "ep1", 0, 10)
	if err != nil {
		t.Fatalf("ReadJournalEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 journal events in store, got %d", len(events))
	}
	for i, expectedID := range []string{"gw_poll_001", "gw_poll_002", "gw_poll_003"} {
		if events[i].EventType != "poll.completed" {
			t.Errorf("event %d: expected event type 'poll.completed', got %q", i, events[i].EventType)
		}
		if events[i].AggregateID != expectedID {
			t.Errorf("event %d: expected aggregateID %q, got %q", i, expectedID, events[i].AggregateID)
		}
	}

	// Verify hub received all 3 events
	for i := 1; i <= 3; i++ {
		select {
		case ev := <-ch:
			expectedID := fmt.Sprintf("gw_poll_%03d", i)
			if ev.EventType != "poll.completed" {
				t.Errorf("hub event %d: expected 'poll.completed', got %q", i, ev.EventType)
			}
			if ev.AggregateID != expectedID {
				t.Errorf("hub event %d: expected aggregateID %q, got %q", i, expectedID, ev.AggregateID)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for hub event %d", i)
		}
	}
}
