package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func TestGatewayLivePreviewAndCanaryEntrypoint(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "gateway_test.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()

	cfg := config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		DatabasePath:       dbPath,
	}
	srv := httpapi.New(cfg, store)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	// 1. Fetch CSRF token
	csrfResp, err := client.Get(ts.URL + "/api/v1/csrf")
	if err != nil {
		t.Fatalf("GET /csrf failed: %v", err)
	}
	defer csrfResp.Body.Close()
	var csrfData struct{ Token string }
	if err := json.NewDecoder(csrfResp.Body).Decode(&csrfData); err != nil {
		t.Fatalf("decode csrf failed: %v", err)
	}

	modes := []string{"standard", "reference", "hybrid"}
	testToken := "LIVE_CANARY_TEST_99"

	for _, mode := range modes {
		previewReqBody, _ := json.Marshal(map[string]string{
			"accountNumber": "97041612345678",
			"accountName":   "NGUYEN VAN LIVE",
			"mode":          mode,
			"testId":        testToken,
			"host":          "gateway.live.test",
		})

		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/payment-qr/preview", bytes.NewReader(previewReqBody))
		if err != nil {
			t.Fatalf("NewRequest preview failed: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", ts.URL)
		req.Header.Set("X-CSRF-Token", csrfData.Token)
		for _, c := range csrfResp.Cookies() {
			req.AddCookie(c)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /preview failed for mode %s: %v", mode, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for preview mode %s, got %d", mode, resp.StatusCode)
		}

		var previewRes map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&previewRes); err != nil {
			t.Fatalf("decode preview response failed: %v", err)
		}
		resp.Body.Close()

		payload, _ := previewRes["payload"].(string)
		img, _ := previewRes["image"].(string)
		crcValid, _ := previewRes["crcValid"].(bool)

		if payload == "" {
			t.Fatalf("mode %s: empty payload", mode)
		}
		if !strings.HasPrefix(img, "data:image/png;base64,") {
			t.Fatalf("mode %s: invalid image format", mode)
		}
		if !crcValid {
			t.Fatalf("mode %s: invalid CRC", mode)
		}

		t.Logf("Live preview mode=%s success: payload_len=%d crcValid=%v", mode, len(payload), crcValid)
	}

	// 2. Hit public canary endpoint
	canaryReq, err := http.NewRequest(http.MethodGet, ts.URL+"/api/public/v1/payment-qr/canary/"+testToken, nil)
	if err != nil {
		t.Fatalf("NewRequest canary failed: %v", err)
	}
	canaryReq.Header.Set("User-Agent", "ACB_ONE_MobileApp/2026.09 (iOS 18; iPhone)")
	canaryResp, err := client.Do(canaryReq)
	if err != nil {
		t.Fatalf("GET canary failed: %v", err)
	}
	defer canaryResp.Body.Close()
	if canaryResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from canary hit, got %d", canaryResp.StatusCode)
	}
	var canaryRes map[string]any
	_ = json.NewDecoder(canaryResp.Body).Decode(&canaryRes)
	if canaryRes["status"] != "recorded" {
		t.Fatalf("expected status 'recorded', got %v", canaryRes["status"])
	}
	t.Logf("Canary hit recorded: token=%s status=%v", testToken, canaryRes["status"])

	// 3. Inspect canary status via admin endpoint
	statusReq, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/payment-qr/canary/"+testToken, nil)
	if err != nil {
		t.Fatalf("NewRequest canary status failed: %v", err)
	}
	for _, c := range csrfResp.Cookies() {
		statusReq.AddCookie(c)
	}
	statusResp, err := client.Do(statusReq)
	if err != nil {
		t.Fatalf("GET canary status failed: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from canary status inspection, got %d", statusResp.StatusCode)
	}
	var statusData map[string]any
	_ = json.NewDecoder(statusResp.Body).Decode(&statusData)
	hitCount, _ := statusData["hitCount"].(float64)
	if int(hitCount) != 1 {
		t.Fatalf("expected hitCount 1, got %v", hitCount)
	}
	lastUA, _ := statusData["lastUserAgent"].(string)
	if !strings.Contains(lastUA, "ACB_ONE_MobileApp") {
		t.Fatalf("expected lastUserAgent to contain ACB_ONE_MobileApp, got %s", lastUA)
	}
	t.Logf("Canary inspection verified: token=%s hitCount=%d lastUA=%s", testToken, int(hitCount), lastUA)
}
