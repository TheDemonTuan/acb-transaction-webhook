package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/webhook"
)

func TestE2ERealtimeEventDrivenPipeline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***9999")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING', generation=1 WHERE id=?", conn.ID)

	// Mock webhook receiver
	receivedCh := make(chan []byte, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		select {
		case receivedCh <- body:
		default:
		}
	}))
	defer ts.Close()

	ep, err := store.CreateEndpointWithSecret(ctx, "Pipeline Hook", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, ep.ID, "ACTIVE")

	hub := eventhub.New()
	_, hubCh, cancelHub := hub.Subscribe()
	defer cancelHub()

	dispatcher := webhook.NewDispatcher(store, ts.Client()).SetSkipURLValidation(true)
	go dispatcher.Run(ctx)

	mockHTML := fmt.Sprintf(`
<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="fixture-state"><input name="dse_sessionId" value="fixture-session"></form>
<table>
  <tr><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
  <tr><td>%s</td><td>9988</td><td>-</td><td>200.000</td><td>5.000.000</td><td>Tien Luong</td></tr>
</table>`, time.Now().In(acb.DefaultLocation).Format("02/01/2006"))

	mockClient := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.HistoryPage,
			Body:       mockHTML,
		},
		historyResp: acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHTML},
	}

	m := New(store, mockClient, 10*time.Second, 10*time.Second)
	m.WithEventNotifier(func(events []storage.EventNotification) {
		for _, ev := range events {
			hub.Publish(eventhub.Event{
				Seq:         ev.JournalSeq,
				Epoch:       ev.Epoch,
				EventType:   ev.EventType,
				AggregateID: ev.TransactionID,
				Payload:     ev.Payload,
				CreatedAt:   ev.CreatedAt,
			})
		}
		dispatcher.Wake()
	})

	// Execute poll cycle and measure pipeline latency
	start := time.Now()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	// 1. Verify EventHub received live event
	select {
	case ev := <-hubCh:
		if ev.EventType != "bank.transaction.credit" {
			t.Fatalf("unexpected event type: %s", ev.EventType)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for event on EventHub")
	}

	// 2. Verify Webhook dispatcher woke immediately and delivered HTTP request
	select {
	case body := <-receivedCh:
		elapsed := time.Since(start)
		if len(body) == 0 {
			t.Fatal("received empty webhook body")
		}
		// Under test environment, full pipeline completed!
		t.Logf("Full pipeline completed in: %v", elapsed)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}

	// 3. Wait for delivery status in DB to be marked DELIVERED
	dbDeadline := time.Now().Add(1 * time.Second)
	var deliveredCount int
	for time.Now().Before(dbDeadline) {
		_ = store.DB().QueryRowContext(ctx, "SELECT count(*) FROM deliveries WHERE status='DELIVERED'").Scan(&deliveredCount)
		if deliveredCount == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if deliveredCount != 1 {
		t.Fatalf("expected 1 DELIVERED delivery in DB, got %d", deliveredCount)
	}

	summary, err := store.DeliverySummary(ctx)
	if err != nil || summary.Pending != 0 {
		t.Fatalf("expected 0 pending deliveries, got %+v %v", summary, err)
	}

	// 4. Verify journal entry created
	var journalCount int
	_ = store.DB().QueryRowContext(ctx, "SELECT count(*) FROM event_journal").Scan(&journalCount)
	if journalCount != 1 {
		t.Fatalf("expected 1 event_journal entry, got %d", journalCount)
	}
}
