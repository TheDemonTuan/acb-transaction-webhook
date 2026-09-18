package integration_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/monitor"
	"github.com/thedemontuan/acb-transaction-webhook/internal/realtimestream"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type blockedACBClient struct {
	page2Started chan struct{}
	unblockPage2 chan struct{}
	once         sync.Once
	page         int
}

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

func (c *blockedACBClient) Bootstrap(context.Context) (acb.Response, error) {
	return acb.Response{StatusCode: http.StatusOK, Kind: acb.AccountDetailPage, Body: `<form action="/history" method="POST"><input type="hidden" name="dse_operationName" value="op1"/><input type="hidden" name="dse_processorState" value="ps1"/><input type="hidden" name="AccountNbr" value="123456"/></form>`}, nil
}

func (c *blockedACBClient) History(ctx context.Context, _ string, _ map[string]string) (acb.Response, error) {
	c.page++
	if c.page == 2 {
		c.once.Do(func() { close(c.page2Started) })
		select {
		case <-ctx.Done():
			return acb.Response{}, ctx.Err()
		case <-c.unblockPage2:
		}
	}
	next := `<span class="disabled">Trang sau</span>`
	if c.page == 1 {
		next = `<a href="/history?page=next" onclick="submitEvent('nextPage')">Trang sau</a>`
	}
	body := fmt.Sprintf(`<form action="/history" method="POST"><input type="hidden" name="dse_operationName" value="op1"/><input type="hidden" name="dse_processorState" value="ps%d"/><input type="hidden" name="AccountNbr" value="123456"/></form><table><tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr><tr><td>PIPELINE_PAGE_%d</td><td>15/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Pipeline %d</td></tr><tr><td colspan="6">%s</td></tr></table>`, c.page, c.page, c.page, next)
	return acb.Response{StatusCode: http.StatusOK, Kind: acb.HistoryPage, Body: body}, nil
}

func TestRealtimePipelineDeliversPage1BeforePage2Completes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "realtime_pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
		t.Fatal(err)
	}

	workerHub := eventhub.New()
	internalServer := httptest.NewServer(realtimestream.NewServer(realtimestream.ServerConfig{Hub: workerHub, Token: "pipeline-secret", Heartbeat: 20 * time.Millisecond}))
	defer internalServer.Close()
	defer internalServer.CloseClientConnections()
	gatewayHub := eventhub.New()
	gateway := httpapi.New(config.Config{DevelopmentSubject: "pipeline@example.com"}, store).WithEventHub(gatewayHub)
	coordinator := httpapi.NewRealtimeCoordinator(gateway, time.Hour)
	gateway.WithRealtimeSubmit(coordinator.Submit)
	go coordinator.Run(ctx)
	waitCoordinator(t, coordinator, 0)

	connected := make(chan struct{})
	var connectOnce sync.Once
	streamClient, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL: internalServer.URL,
		Token:   "pipeline-secret",
		OnConnect: func() {
			connectOnce.Do(func() { close(connected) })
			coordinator.RequestReconcile()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = streamClient.Run(ctx, coordinator.Submit) }()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("internal stream did not connect")
	}

	publicServer := httptest.NewServer(gateway.Handler())
	defer publicServer.Close()
	defer publicServer.CloseClientConnections()
	publicEvents := make(chan string, 8)
	go readPublicSSE(ctx, t, publicServer.URL+"/api/v1/events", publicEvents)
	select {
	case initial := <-publicEvents:
		if !strings.Contains(initial, "event: initial_state") {
			t.Fatalf("expected initial state, got %q", initial)
		}
	case <-ctx.Done():
		t.Fatal("public stream did not initialize")
	}

	page2Started := make(chan struct{})
	unblockPage2 := make(chan struct{})
	bank := &blockedACBClient{page2Started: page2Started, unblockPage2: unblockPage2}
	bankMonitor := monitor.New(store, bank, time.Second, time.Second)
	bankMonitor.WithEventNotifier(func(events []storage.EventNotification) {
		for _, event := range events {
			workerHub.Publish(eventhub.Event{Seq: event.JournalSeq, Epoch: event.Epoch, EventType: event.EventType, AggregateID: event.TransactionID, Payload: event.Payload, CreatedAt: event.CreatedAt, CommittedAt: event.CommittedAt})
		}
	})
	taskDone := make(chan error, 1)
	go func() {
		result, stepErr := monitor.NewRealtimeTask(bankMonitor, monitor.PriorityRealtimePoll, conn.ID, conn.Generation).Step(ctx)
		if stepErr == nil {
			stepErr = result.Error
		}
		taskDone <- stepErr
	}()
	select {
	case <-page2Started:
	case <-ctx.Done():
		t.Fatal("page 2 was not requested")
	}

	select {
	case frame := <-publicEvents:
		if !strings.Contains(frame, "event: bank.transaction.credit") || !strings.Contains(frame, "PIPELINE_PAGE_1") {
			t.Fatalf("unexpected public event: %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("page 1 did not reach public SSE while page 2 was blocked")
	}
	txns, err := store.ListTransactions(ctx, 10)
	if err != nil || len(txns) != 1 || !strings.Contains(txns[0].Description, "Pipeline 1") {
		t.Fatalf("page 1 was not committed before page 2 release: transactions=%+v err=%v", txns, err)
	}
	close(unblockPage2)
	select {
	case err := <-taskDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("realtime task did not complete")
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
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
		t.Fatal(err)
	}

	workerHub := eventhub.New()
	internalHandler := newReconnectGateHandler(realtimestream.NewServer(realtimestream.ServerConfig{
		Hub: workerHub, Token: "pipeline-secret", Heartbeat: 20 * time.Millisecond,
	}))
	internalServer := httptest.NewServer(internalHandler)
	defer internalServer.Close()
	defer internalServer.CloseClientConnections()

	gatewayHub := eventhub.New()
	gateway := httpapi.New(config.Config{DevelopmentSubject: "pipeline@example.com"}, store).WithEventHub(gatewayHub)
	coordinator := httpapi.NewRealtimeCoordinator(gateway, time.Hour)
	gateway.WithRealtimeSubmit(coordinator.Submit)
	go coordinator.Run(ctx)
	waitCoordinator(t, coordinator, 0)

	warmupPayload := []byte(`{"ready":true}`)
	warmupSeq, err := store.AppendJournalEvent(ctx, "ep1", "test.ready", "ready", warmupPayload)
	if err != nil {
		t.Fatal(err)
	}

	connected := make(chan struct{}, 2)
	disconnected := make(chan struct{}, 1)
	streamClient, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL:        internalServer.URL,
		Token:          "pipeline-secret",
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
		OnConnect: func() {
			coordinator.RequestReconcile()
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
	go func() { streamDone <- streamClient.Run(ctx, coordinator.Submit) }()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("internal stream did not connect")
	}
	waitCoordinator(t, coordinator, warmupSeq)
	syncPayload := []byte(`{"synchronized":true}`)
	syncSeq, err := store.AppendJournalEvent(ctx, "ep1", "test.synchronized", "synchronized", syncPayload)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Submit(eventhub.Event{Seq: syncSeq, Epoch: "ep1", EventType: "test.synchronized", AggregateID: "synchronized", Payload: syncPayload}); err != nil {
		t.Fatal(err)
	}
	waitCoordinator(t, coordinator, syncSeq)

	publicServer := httptest.NewServer(gateway.Handler())
	defer publicServer.Close()
	defer publicServer.CloseClientConnections()
	publicEvents := make(chan string, 16)
	go readPublicSSE(ctx, t, publicServer.URL+"/api/v1/events", publicEvents)
	select {
	case initial := <-publicEvents:
		if !strings.Contains(initial, "event: initial_state") {
			t.Fatalf("expected initial state, got %q", initial)
		}
	case <-ctx.Done():
		t.Fatal("public SSE did not initialize")
	}

	internalHandler.disconnect()
	select {
	case <-disconnected:
	case <-ctx.Done():
		t.Fatal("internal stream did not disconnect")
	}

	page2Started := make(chan struct{})
	unblockPage2 := make(chan struct{})
	bank := &blockedACBClient{page2Started: page2Started, unblockPage2: unblockPage2}
	bankMonitor := monitor.New(store, bank, time.Second, time.Second)
	bankMonitor.WithEventNotifier(func(events []storage.EventNotification) {
		for _, event := range events {
			workerHub.Publish(eventhub.Event{Seq: event.JournalSeq, Epoch: event.Epoch, EventType: event.EventType, AggregateID: event.TransactionID, Payload: event.Payload, CreatedAt: event.CreatedAt, CommittedAt: event.CommittedAt})
		}
	})
	taskDone := make(chan error, 1)
	go func() {
		result, stepErr := monitor.NewRealtimeTask(bankMonitor, monitor.PriorityRealtimePoll, conn.ID, conn.Generation).Step(ctx)
		if stepErr == nil {
			stepErr = result.Error
		}
		taskDone <- stepErr
	}()
	select {
	case <-page2Started:
	case <-ctx.Done():
		t.Fatal("page 2 was not requested")
	}

	txns, err := store.ListTransactions(ctx, 10)
	if err != nil || len(txns) != 1 || !strings.Contains(txns[0].Description, "Pipeline 1") {
		t.Fatalf("transaction was not committed during outage: transactions=%+v err=%v", txns, err)
	}
	entries, err := store.ReadJournalEvents(ctx, "ep1", syncSeq, 10)
	if err != nil || len(entries) != 1 || entries[0].EventType != "bank.transaction.credit" {
		t.Fatalf("credit journal row missing during outage: entries=%+v err=%v", entries, err)
	}
	target := entries[0]
	if got := coordinator.LastSeq(); got >= target.Seq {
		t.Fatalf("coordinator advanced during disconnected stream: lastSeq=%d target=%d", got, target.Seq)
	}

	internalHandler.allowReconnect()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("internal stream did not reconnect")
	}
	var creditFrames []string
	select {
	case frame := <-publicEvents:
		creditFrames = append(creditFrames, frame)
		if !strings.Contains(frame, "id: ep1:"+strconv.FormatInt(target.Seq, 10)) || !strings.Contains(frame, "event: bank.transaction.credit") || !strings.Contains(frame, "PIPELINE_PAGE_1") {
			t.Fatalf("unexpected recovered public event: %q", frame)
		}
		if strings.Contains(frame, "AccountNbr") || strings.Contains(frame, "123456") {
			t.Fatalf("recovered payload leaked unmasked bank account data: %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("committed transaction was not recovered after reconnect")
	}

	workerHub.Publish(eventhub.Event{Seq: target.Seq, Epoch: target.Epoch, EventType: target.EventType, AggregateID: target.AggregateID, Payload: target.Payload, CreatedAt: target.CreatedAt})
	barrierPayload := []byte(`{"barrier":true}`)
	barrierSeq, err := store.AppendJournalEvent(ctx, "ep1", "test.barrier", "barrier", barrierPayload)
	if err != nil {
		t.Fatal(err)
	}
	workerHub.Publish(eventhub.Event{Seq: barrierSeq, Epoch: "ep1", EventType: "test.barrier", AggregateID: "barrier", Payload: barrierPayload})
	for {
		select {
		case frame := <-publicEvents:
			if strings.Contains(frame, "id: ep1:"+strconv.FormatInt(target.Seq, 10)) {
				creditFrames = append(creditFrames, frame)
			}
			if strings.Contains(frame, "id: ep1:"+strconv.FormatInt(barrierSeq, 10)) {
				if len(creditFrames) != 1 {
					t.Fatalf("expected recovered credit exactly once before barrier, got %d frames: %q", len(creditFrames), creditFrames)
				}
				close(unblockPage2)
				select {
				case err := <-taskDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("realtime task did not complete")
				}
				cancel()
				select {
				case err := <-streamDone:
					if err != nil && err != context.Canceled {
						t.Fatalf("stream client stopped unexpectedly: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("stream client did not stop")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("barrier event did not reach public SSE")
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
