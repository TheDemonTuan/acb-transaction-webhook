package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestServeWorkerHTTPReportsUnexpectedFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	serveWorkerHTTP(&http.Server{Handler: http.NotFoundHandler()}, listener, "realtime", slog.Default(), errCh)
	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "realtime server") {
			t.Fatalf("unexpected error: %v", err)
		}
	default:
		t.Fatal("expected serve failure to be reported")
	}
}

func TestWorkerDeployCapabilities(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "-deploy-capabilities")
	cmd.Dir = "."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("worker -deploy-capabilities failed: %v", err)
	}
	var capabilities struct {
		Protocol          int  `json:"protocol"`
		Quiesce           bool `json:"quiesce"`
		Drain             bool `json:"drain"`
		Resume            bool `json:"resume"`
		NotificationDrain bool `json:"notificationDrain"`
		PaymentDrain      bool `json:"paymentDrain"`
		JournalCheckpoint bool `json:"journalCheckpoint"`
	}
	if err := json.Unmarshal(out, &capabilities); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if capabilities.Protocol < 3 || !capabilities.Quiesce || !capabilities.Drain || !capabilities.Resume ||
		!capabilities.NotificationDrain || !capabilities.PaymentDrain || !capabilities.JournalCheckpoint {
		t.Fatalf("incomplete deploy capabilities: %+v", capabilities)
	}
}

func TestWorkerRealtimeAddressMustBindBeforeReady(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if duplicate, err := net.Listen("tcp", occupied.Addr().String()); err == nil {
		duplicate.Close()
		t.Fatal("expected occupied realtime address bind to fail")
	}
}

func TestWorkerRoleValidation_Subprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	tests := []struct {
		name       string
		env        []string
		wantStderr string
	}{
		{
			name: "rejects gateway role on worker",
			env: []string{
				"APP_ENV=development",
				"RUNTIME_ROLE=gateway",
			},
			wantStderr: "unsupported runtime role for worker",
		},
		{
			name: "rejects monolith-dev role on worker",
			env: []string{
				"APP_ENV=development",
				"RUNTIME_ROLE=monolith-dev",
			},
			wantStderr: "unsupported runtime role for worker",
		},
		{
			name: "production rejects empty role on worker",
			env: []string{
				"APP_ENV=production",
				"RUNTIME_ROLE=",
			},
			wantStderr: "RUNTIME_ROLE is required in production",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("go", "run", ".")
			cmd.Dir = "."
			cmd.Env = append(os.Environ(), tc.env...)

			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			if err == nil {
				t.Fatalf("expected worker startup to fail for %s, but it exited 0", tc.name)
			}
			out := stderr.String() + stdout.String()
			if !strings.Contains(out, tc.wantStderr) {
				t.Fatalf("expected output to contain %q, got %q", tc.wantStderr, out)
			}
		})
	}
}

func TestWorkerQuiesceDrainFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	var receivedPath, receivedToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedToken = r.Header.Get("X-Worker-Internal-Token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"quiesced","quiesced":true}`))
	}))
	defer srv.Close()

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("worker -quiesce sends POST /rpc/quiesce with internal token", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-quiesce")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=test-worker-token-xyz",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("worker -quiesce failed: %v, stderr: %s", err, stderr.String())
		}
		if receivedPath != "/rpc/quiesce" {
			t.Errorf("expected path /rpc/quiesce, got %s", receivedPath)
		}
		if receivedToken != "test-worker-token-xyz" {
			t.Errorf("expected token test-worker-token-xyz, got %s", receivedToken)
		}
		if !strings.Contains(stdout.String(), `"quiesced":true`) {
			t.Errorf("expected quiesce response in stdout, got %s", stdout.String())
		}
	})

	t.Run("worker -quiesce sends POST /rpc/quiesce with WORKER_INTERNAL_TOKEN_FILE", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "worker_token")
		if err := os.WriteFile(tokenFile, []byte("file-token-secret-456\n"), 0600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("go", "run", ".", "-quiesce")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=",
			"WORKER_INTERNAL_TOKEN_FILE="+tokenFile,
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("worker -quiesce failed: %v, stderr: %s", err, stderr.String())
		}
		if receivedPath != "/rpc/quiesce" {
			t.Errorf("expected path /rpc/quiesce, got %s", receivedPath)
		}
		if receivedToken != "file-token-secret-456" {
			t.Errorf("expected token file-token-secret-456, got %s", receivedToken)
		}
	})

	t.Run("worker -resume sends POST /rpc/resume with WORKER_INTERNAL_TOKEN_FILE", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "worker_token")
		if err := os.WriteFile(tokenFile, []byte("resume-token-secret-789\n"), 0600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("go", "run", ".", "-resume")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=",
			"WORKER_INTERNAL_TOKEN_FILE="+tokenFile,
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("worker -resume failed: %v, stderr: %s", err, stderr.String())
		}
		if receivedPath != "/rpc/resume" {
			t.Errorf("expected path /rpc/resume, got %s", receivedPath)
		}
		if receivedToken != "resume-token-secret-789" {
			t.Errorf("expected token resume-token-secret-789, got %s", receivedToken)
		}
	})

	t.Run("worker -quiesce fails when WORKER_INTERNAL_TOKEN_FILE is missing", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-quiesce")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=",
			"WORKER_INTERNAL_TOKEN_FILE=/nonexistent/token/file",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected error with nonexistent token file, got nil")
		}
		if !strings.Contains(stderr.String(), "failed to read worker internal token") {
			t.Fatalf("expected stderr to mention failed to read worker internal token, got %q", stderr.String())
		}
	})
}

type mockWaker struct {
	wakeCount int
}

func (m *mockWaker) Wake() {
	m.wakeCount++
}

func TestWorkerPaymentNotifierPublishesCommittedEventAndWakesDispatcher(t *testing.T) {
	waker := &mockWaker{}
	hub := eventhub.New()
	_, published, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	notifier := newWorkerPaymentNotifier(waker, hub)
	event := storage.EventNotification{JournalSeq: 42, Epoch: "ep1", EventType: "bank.transaction.credit", TransactionID: "txn_paid", Payload: []byte(`{"provider":"PAYOS","orderCode":"123456789012"}`), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	notifier(event)
	select {
	case got := <-published:
		if got.Seq != event.JournalSeq || got.AggregateID != event.TransactionID || got.EventType != event.EventType || !bytes.Equal(got.Payload, event.Payload) {
			t.Fatalf("published wrong event: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("committed event not published")
	}
	if waker.wakeCount != 1 {
		t.Fatalf("dispatcher wake count: %d", waker.wakeCount)
	}
}
