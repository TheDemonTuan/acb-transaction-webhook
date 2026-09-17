package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type adaptiveTestBankClient struct {
	historyCalled atomic.Int64
}

func (c *adaptiveTestBankClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	html := `<table>
  <tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
  <tr><td>17/09/2026</td><td>17/09/2026</td><td>998811</td><td>-</td><td>500.000</td><td>5.000.000</td><td>Thanh toan don hang #101</td></tr>
</table>`
	return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: html}, nil
}

func (c *adaptiveTestBankClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	c.historyCalled.Add(1)
	html := `<table>
  <tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
  <tr><td>17/09/2026</td><td>17/09/2026</td><td>998811</td><td>-</td><td>500.000</td><td>5.000.000</td><td>Thanh toan don hang #101</td></tr>
</table>`
	return acb.Response{StatusCode: 200, Body: html}, nil
}

type integrationWorkerHandler struct {
	mon *monitor.Monitor
}

func (h *integrationWorkerHandler) RequestSync(ctx context.Context) error {
	return h.mon.RequestSync(ctx)
}
func (h *integrationWorkerHandler) CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error) {
	return storage.HistorySyncJob{}, nil
}
func (h *integrationWorkerHandler) CancelHistoryJob(ctx context.Context, jobID string) error {
	return nil
}
func (h *integrationWorkerHandler) NotifySettingsChanged(ctx context.Context) error {
	h.mon.NotifySettingsChanged()
	return nil
}
func (h *integrationWorkerHandler) WakeDispatcher(ctx context.Context) error { return nil }
func (h *integrationWorkerHandler) VerifySession(ctx context.Context, account string, generation int64, password []byte) error {
	return nil
}
func (h *integrationWorkerHandler) TestNotificationChannel(ctx context.Context, channelID string) (workerrpc.TestNotificationResponse, error) {
	return workerrpc.TestNotificationResponse{Success: true}, nil
}
func (h *integrationWorkerHandler) Quiesce(ctx context.Context) (workerrpc.QuiesceResponse, error) {
	return workerrpc.QuiesceResponse{Status: "quiesced"}, nil
}
func (h *integrationWorkerHandler) Resume(ctx context.Context) error { return nil }
func (h *integrationWorkerHandler) ActivatePaymentWindow(ctx context.Context) (workerrpc.PaymentWindowResponse, error) {
	profile := h.mon.ActivatePaymentWindow()
	var nextPhaseStr string
	if !profile.NextPhaseAt.IsZero() {
		nextPhaseStr = profile.NextPhaseAt.UTC().Format(time.RFC3339)
	}
	return workerrpc.PaymentWindowResponse{
		Phase:       string(profile.Phase),
		NextPhaseAt: nextPhaseStr,
	}, nil
}

func TestAdaptivePolling_EndToEndPipeline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "adaptive_e2e.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***9999")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	bankClient := &adaptiveTestBankClient{}
	mon := monitor.New(store, bankClient, 20*time.Second, 30*time.Second)

	hub := eventhub.New()
	mon.WithEventNotifier(func(events []storage.EventNotification) {
		for _, e := range events {
			hub.Publish(eventhub.Event{
				EventType:   e.EventType,
				Payload:     e.Payload,
				Epoch:       e.Epoch,
				Seq:         e.JournalSeq,
				AggregateID: e.TransactionID,
				CreatedAt:   e.CreatedAt,
				CommittedAt: e.CommittedAt,
			})
		}
	})

	// Setup Worker RPC
	workerHandler := &integrationWorkerHandler{mon: mon}
	rpcServer, err := workerrpc.NewServer(workerHandler, "worker-token-test-123")
	if err != nil {
		t.Fatal(err)
	}
	rpcTS := httptest.NewServer(rpcServer.Handler())
	defer rpcTS.Close()

	// Setup Gateway with worker RPC client adapter
	workerClient := workerrpc.NewClient(rpcTS.URL, "worker-token-test-123")
	cfg := config.Config{Production: false}
	gwServer := httpapi.New(cfg, store).
		WithEventHub(hub).
		WithPaymentActivator(httpapi.PaymentWindowActivatorFunc(func(ctx context.Context) (httpapi.PaymentActivationState, error) {
			resp, err := workerClient.ActivatePaymentWindow(ctx)
			if err != nil {
				return httpapi.PaymentActivationState{}, err
			}
			var nextPhase time.Time
			if resp.NextPhaseAt != "" {
				nextPhase, _ = time.Parse(time.RFC3339, resp.NextPhaseAt)
			}
			return httpapi.PaymentActivationState{
				TrackingActive: resp.Phase == "GRACE" || resp.Phase == "HOT" || resp.Phase == "WARM" || resp.Phase == "COOL",
				Phase:          resp.Phase,
				NextPhaseAt:    nextPhase,
			}, nil
		}))

	gwTS := httptest.NewServer(gwServer.Handler())
	defer gwTS.Close()

	// Connect SSE client to gateway
	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, gwTS.URL+"/api/public/v1/events/stream", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE connection failed: %v", err)
	}
	defer sseResp.Body.Close()

	// 1. Client triggers payment QR activation via public Gateway
	activateReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwTS.URL+"/api/public/v1/payment-qr/activate", strings.NewReader(`{"identifier": "customer_qr_scan_001"}`))
	activateReq.Header.Set("Content-Type", "application/json")
	activateResp, err := http.DefaultClient.Do(activateReq)
	if err != nil {
		t.Fatalf("activate request failed: %v", err)
	}
	defer activateResp.Body.Close()

	if activateResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from activation, got %d", activateResp.StatusCode)
	}
	if cc := activateResp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control: no-store, got %q", cc)
	}

	var activateData map[string]any
	_ = json.NewDecoder(activateResp.Body).Decode(&activateData)
	if activateData["trackingActive"] != true || activateData["phase"] != "GRACE" {
		t.Fatalf("unexpected activation payload: %v", activateData)
	}

	// 2. Verify worker monitor state entered GRACE
	boostProfile := mon.ResolvePollBoost(time.Now())
	if boostProfile.Phase != monitor.PollBoostGrace {
		t.Fatalf("expected monitor boost to be GRACE, got %v", boostProfile.Phase)
	}

	// 3. Trigger single poll cycle through monitor
	if err := mon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	// 4. Verify SSE client receives the credited transaction event
	sseReader := bufio.NewReader(sseResp.Body)
	receivedEvent := false
	timeout := time.After(3 * time.Second)

	for !receivedEvent {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for SSE credit transaction event")
		default:
			line, err := sseReader.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE read error: %v", err)
			}
			if strings.HasPrefix(line, "data:") {
				dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if strings.Contains(dataStr, "998811") || strings.Contains(dataStr, "500.000") {
					receivedEvent = true
				}
			}
		}
	}

	if !receivedEvent {
		t.Fatal("expected credited transaction to arrive over SSE stream")
	}
}
