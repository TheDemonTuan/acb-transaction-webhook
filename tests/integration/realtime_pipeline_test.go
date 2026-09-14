package integration_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	gatewayHub := eventhub.New()
	gateway := httpapi.New(config.Config{DevelopmentSubject: "pipeline@example.com"}, store).WithEventHub(gatewayHub)
	coordinator := httpapi.NewRealtimeCoordinator(gateway, time.Hour)
	gateway.WithRealtimeInput(coordinator.Input(), coordinator.RequestReconcile)
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
	defer publicServer.CloseClientConnections()
	defer publicServer.Close()
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

func readPublicSSE(ctx context.Context, t *testing.T, url string, frames chan<- string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	var frame strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if frame.Len() > 0 {
				select {
				case frames <- frame.String():
				case <-ctx.Done():
					return
				}
				frame.Reset()
			}
			continue
		}
		frame.WriteString(line)
		frame.WriteByte('\n')
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
