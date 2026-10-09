package integration_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/realtimestream"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type reconnectGateHandler struct {
	handler http.Handler
	gate    chan struct{}

	mu            sync.Mutex
	requests      int
	firstCancel   context.CancelFunc
	firstFinished chan struct{}
	openOnce      sync.Once
}

func newReconnectGateHandler(handler http.Handler) *reconnectGateHandler {
	return &reconnectGateHandler{handler: handler, gate: make(chan struct{}), firstFinished: make(chan struct{})}
}

func (h *reconnectGateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests++
	first := h.requests == 1
	h.mu.Unlock()

	if first {
		ctx, cancel := context.WithCancel(r.Context())
		h.mu.Lock()
		h.firstCancel = cancel
		h.mu.Unlock()
		h.handler.ServeHTTP(w, r.WithContext(ctx))
		close(h.firstFinished)
		return
	}

	select {
	case <-h.gate:
		h.handler.ServeHTTP(w, r)
	case <-r.Context().Done():
	}
}

func (h *reconnectGateHandler) disconnect() {
	h.mu.Lock()
	cancel := h.firstCancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-h.firstFinished
}

func (h *reconnectGateHandler) allowReconnect() {
	h.openOnce.Do(func() { close(h.gate) })
}

func TestRealtimePipelineDeliversIndependentPayOSOrders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "realtime_pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ep, err := store.CreateEndpointWithSecret(ctx, "Pipeline webhook", "https://example.test/pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	workerHub := eventhub.New()
	f := newPaymentFixture(t, store, func(event storage.EventNotification) {
		var receipts int
		if err := store.DB().QueryRow(`SELECT count(*) FROM payment_receipts WHERE transaction_id=?`, event.TransactionID).Scan(&receipts); err != nil || receipts != 1 {
			t.Errorf("published before atomic settlement: receipts=%d err=%v", receipts, err)
		}
		workerHub.Publish(eventhub.Event{Seq: event.JournalSeq, Epoch: event.Epoch, EventType: event.EventType, AggregateID: event.TransactionID, Payload: event.Payload, CreatedAt: event.CreatedAt, CommittedAt: event.CommittedAt})
	})
	internalServer := httptest.NewServer(realtimestream.NewServer(realtimestream.ServerConfig{Hub: workerHub, Token: "pipeline-secret", Heartbeat: 20 * time.Millisecond}))
	defer internalServer.Close()
	gateway := httpapi.New(config.Config{DevelopmentSubject: "pipeline@example.com"}, store).WithEventHub(eventhub.New())
	coordinator := httpapi.NewRealtimeCoordinator(gateway, time.Hour)
	gateway.WithRealtimeSubmit(coordinator.Submit)
	go coordinator.Run(ctx)
	waitCoordinator(t, coordinator, 0)
	connected := make(chan struct{}, 1)
	client, err := realtimestream.NewClient(realtimestream.ClientConfig{BaseURL: internalServer.URL, Token: "pipeline-secret", OnConnect: func() {
		coordinator.RequestReconcile()
		connected <- struct{}{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = client.Run(ctx, coordinator.Submit) }()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("internal stream did not connect")
	}
	publicServer := httptest.NewServer(gateway.Handler())
	defer publicServer.Close()
	defer cancel()
	frames := make(chan string, 16)
	go readPublicSSE(ctx, t, publicServer.URL+"/api/public/v1/events", frames)
	select {
	case frame := <-frames:
		if !strings.Contains(frame, "event: initial_state") {
			t.Fatalf("unexpected initialization %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("public stream did not initialize")
	}
	orders := []storage.PaymentOrder{createPayment(t, f, 50000, 1), createPayment(t, f, 50000, 2), createPayment(t, f, 120000, 3)}
	for _, index := range []int{1, 0, 2} {
		order := orders[index]
		settlePayment(t, f, order)
		select {
		case frame := <-frames:
			assertPublicPaymentFrame(t, frame, order)
		case <-ctx.Done():
			t.Fatalf("order %d did not reach public SSE", order.OrderCode)
		}
		persisted, err := store.PaymentOrder(ctx, order.ID)
		if err != nil || persisted.Status != "PAID" {
			t.Fatalf("order was not settled: %+v err=%v", persisted, err)
		}
	}
	var count int
	var total int64
	if err := store.DB().QueryRow(`SELECT count(*),sum(credit) FROM transactions`).Scan(&count, &total); err != nil || count != 3 || total != 220000 {
		t.Fatalf("history count=%d total=%d err=%v", count, total, err)
	}
	for _, table := range []string{"payment_receipts", "events", "event_journal", "deliveries"} {
		if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 3 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	cancel()
}

func TestRealtimePipelineRecoversCommitAfterInternalStreamDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "realtime_recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ep, err := store.CreateEndpointWithSecret(ctx, "Recovery webhook", "https://example.test/recovery")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	workerHub := eventhub.New()
	f := newPaymentFixture(t, store, func(event storage.EventNotification) {
		workerHub.Publish(eventhub.Event{Seq: event.JournalSeq, Epoch: event.Epoch, EventType: event.EventType, AggregateID: event.TransactionID, Payload: event.Payload, CreatedAt: event.CreatedAt, CommittedAt: event.CommittedAt})
	})
	internalHandler := newReconnectGateHandler(realtimestream.NewServer(realtimestream.ServerConfig{Hub: workerHub, Token: "pipeline-secret", Heartbeat: 20 * time.Millisecond}))
	internalServer := httptest.NewServer(internalHandler)
	defer internalServer.Close()
	defer internalHandler.allowReconnect()
	// A persisted startup watermark proves coordinator initialization completed.
	syncSeq, err := store.AppendJournalEvent(ctx, "ep1", "test.synchronized", "synchronized", []byte(`{"synchronized":true}`))
	if err != nil {
		t.Fatal(err)
	}
	gateway := httpapi.New(config.Config{DevelopmentSubject: "pipeline@example.com"}, store).WithEventHub(eventhub.New())
	coordinator := httpapi.NewRealtimeCoordinator(gateway, time.Hour)
	gateway.WithRealtimeSubmit(coordinator.Submit)
	go coordinator.Run(ctx)
	waitCoordinator(t, coordinator, syncSeq)
	connected := make(chan struct{}, 8)
	disconnected := make(chan struct{}, 1)
	firstConnection := true
	client, err := realtimestream.NewClient(realtimestream.ClientConfig{BaseURL: internalServer.URL, Token: "pipeline-secret", InitialBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		OnConnect: func() {
			if !firstConnection {
				coordinator.RequestReconcile()
			}
			firstConnection = false
			connected <- struct{}{}
		},
		OnDisconnect: func(error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	streamDone := make(chan error, 1)
	go func() { streamDone <- client.Run(ctx, coordinator.Submit) }()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("internal stream did not connect")
	}
	publicServer := httptest.NewServer(gateway.Handler())
	defer publicServer.Close()
	defer cancel()
	frames := make(chan string, 16)
	go readPublicSSE(ctx, t, publicServer.URL+"/api/public/v1/events", frames)
	select {
	case frame := <-frames:
		if !strings.Contains(frame, "event: initial_state") {
			t.Fatalf("unexpected initialization %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("public stream did not initialize")
	}
	internalHandler.disconnect()
	select {
	case <-disconnected:
	case <-ctx.Done():
		t.Fatal("internal stream did not disconnect")
	}
	order := createPayment(t, f, 50000, 1)
	settlePayment(t, f, order)
	entries, err := store.ReadJournalEvents(ctx, "ep1", syncSeq, 10)
	if err != nil || len(entries) != 1 || entries[0].EventType != "bank.transaction.credit" {
		t.Fatalf("credit journal missing during outage: entries=%+v err=%v", entries, err)
	}
	target := entries[0]
	if got := coordinator.LastSeq(); got >= target.Seq {
		t.Fatalf("coordinator advanced while disconnected: seq=%d target=%d", got, target.Seq)
	}
	internalHandler.allowReconnect()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("internal stream did not reconnect")
	}
	select {
	case frame := <-frames:
		assertPublicPaymentFrame(t, frame, order)
		if !strings.Contains(frame, "id: ep1:"+strconv.FormatInt(target.Seq, 10)) {
			t.Fatalf("recovered frame has wrong cursor: %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("journal commit was not recovered after reconnect")
	}
	// Duplicate callback and repeated internal delivery must not reach consumers twice.
	for range 10 {
		settlePayment(t, f, order)
	}
	workerHub.Publish(eventhub.Event{Seq: target.Seq, Epoch: target.Epoch, EventType: target.EventType, AggregateID: target.AggregateID, Payload: target.Payload, CreatedAt: target.CreatedAt})
	// A second real settlement is the ordering barrier, not an echoed test event.
	second := createPayment(t, f, 120000, 2)
	settlePayment(t, f, second)
	select {
	case frame := <-frames:
		assertPublicPaymentFrame(t, frame, second)
	case <-ctx.Done():
		t.Fatal("second settlement did not reach public SSE")
	}
	for _, table := range []string{"payment_receipts", "transactions", "events", "deliveries"} {
		var count int
		if err := store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 2 {
			t.Fatalf("replay duplicated %s: count=%d err=%v", table, count, err)
		}
	}
	cancel()
	select {
	case err := <-streamDone:
		if err != nil && err != context.Canceled {
			t.Fatalf("stream stopped unexpectedly: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream client did not stop")
	}
}

func assertPublicPaymentFrame(t *testing.T, frame string, order storage.PaymentOrder) {
	t.Helper()
	if !strings.Contains(frame, "event: bank.transaction.credit") || !strings.Contains(frame, strconv.FormatInt(order.OrderCode, 10)) || !strings.Contains(frame, "KienlongBank") || !strings.Contains(frame, "PAYOS") {
		t.Fatalf("unexpected payment credit frame: %q", frame)
	}
	for _, secret := range []string{order.ID, order.PaymentLinkID, order.CheckoutURL, order.AccountNumber, "MAIN-ACCOUNT", "integration-channel", "integration-api", integrationChecksum} {
		if secret != "" && strings.Contains(frame, secret) {
			t.Fatalf("public payment frame leaked %q: %q", secret, frame)
		}
	}
}

func readPublicSSE(ctx context.Context, t *testing.T, url string, frames chan<- string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		select {
		case frames <- "reader error: " + err.Error():
		case <-ctx.Done():
		}
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		select {
		case frames <- "reader error: " + err.Error():
		case <-ctx.Done():
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		select {
		case frames <- "reader error: unexpected status " + resp.Status:
		case <-ctx.Done():
		}
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	var frame strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if frame.Len() > 0 {
				str := frame.String()
				frame.Reset()
				hasEvent := false
				for _, l := range strings.Split(str, "\n") {
					trimmed := strings.TrimSpace(l)
					if strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "data:") || strings.HasPrefix(trimmed, "id:") {
						hasEvent = true
						break
					}
				}
				if !hasEvent {
					continue
				}
				select {
				case frames <- str:
				case <-ctx.Done():
					return
				}
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), ":") {
			continue
		}
		frame.WriteString(line)
		frame.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		select {
		case frames <- "reader error: " + err.Error():
		case <-ctx.Done():
		}
	}
}

func waitCoordinator(t *testing.T, coordinator *httpapi.RealtimeCoordinator, seq int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for coordinator.LastSeq() != seq {
		if time.Now().After(deadline) {
			t.Fatalf("coordinator seq=%d, want %d", coordinator.LastSeq(), seq)
		}
		time.Sleep(time.Millisecond)
	}
}
