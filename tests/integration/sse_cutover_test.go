package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// TestSSECutoverReplay verifies that when Traefik switches routes from Blue to Green,
// an SSE subscriber reconnecting to the new Green slot with Last-Event-ID replays all
// journaled events without loss, in sequence, while asserting X-Platform-Slot headers.
func TestSSECutoverReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "gateway.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("failed to open shared SQLite database: %v", err)
	}
	defer store.Close()

	hubBlue := eventhub.New()
	cfgBlue := config.Config{
		Production:         false,
		DevelopmentSubject: "dev@example.com",
		Slot:               "blue",
		ReleaseCommit:      "commit-blue-1111",
	}
	serverBlue := httpapi.New(cfgBlue, store).WithEventHub(hubBlue)

	hubGreen := eventhub.New()
	cfgGreen := config.Config{
		Production:         false,
		DevelopmentSubject: "dev@example.com",
		Slot:               "green",
		ReleaseCommit:      "commit-green-2222",
	}
	serverGreen := httpapi.New(cfgGreen, store).WithEventHub(hubGreen)

	// Phase 1: Client connects to Blue slot
	reqBlue := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	recBlue := httptest.NewRecorder()

	blueCtx, blueCancel := context.WithCancel(ctx)
	blueDone := make(chan struct{})
	go func() {
		defer close(blueDone)
		serverBlue.Handler().ServeHTTP(recBlue, reqBlue.WithContext(blueCtx))
	}()

	time.Sleep(50 * time.Millisecond)

	// Verify Blue identity headers
	if slot := recBlue.Header().Get("X-Platform-Slot"); slot != "blue" {
		t.Errorf("expected Blue slot header 'blue', got %q", slot)
	}
	if commit := recBlue.Header().Get("X-Release-Commit"); commit != "commit-blue-1111" {
		t.Errorf("expected Blue commit header 'commit-blue-1111', got %q", commit)
	}

	// Phase 2: Insert initial events to SQLite journal
	seq1, err := store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_001", []byte(`{"amount":100000,"id":"txn_001"}`))
	if err != nil {
		t.Fatalf("failed to append event 1: %v", err)
	}
	hubBlue.Publish(eventhub.Event{
		Seq:         seq1,
		Epoch:       "ep1",
		EventType:   "bank.transaction.credit",
		AggregateID: "txn_001",
		Payload:     []byte(`{"amount":100000,"id":"txn_001"}`),
	})

	seq2, err := store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_002", []byte(`{"amount":200000,"id":"txn_002"}`))
	if err != nil {
		t.Fatalf("failed to append event 2: %v", err)
	}
	hubBlue.Publish(eventhub.Event{
		Seq:         seq2,
		Epoch:       "ep1",
		EventType:   "bank.transaction.credit",
		AggregateID: "txn_002",
		Payload:     []byte(`{"amount":200000,"id":"txn_002"}`),
	})

	time.Sleep(50 * time.Millisecond)

	// Disconnect from Blue (simulate cutover)
	blueCancel()
	<-blueDone

	// Phase 3: During cutover, worker appends events 3 and 4 to SQLite while client is disconnected
	_, err = store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_003", []byte(`{"amount":300000,"id":"txn_003"}`))
	if err != nil {
		t.Fatalf("failed to append event 3: %v", err)
	}
	_, err = store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_004", []byte(`{"amount":400000,"id":"txn_004"}`))
	if err != nil {
		t.Fatalf("failed to append event 4: %v", err)
	}

	// Phase 4: Client reconnects to Green slot with Last-Event-ID: ep1:2
	reqGreen := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	reqGreen.Header.Set("Last-Event-ID", fmt.Sprintf("ep1:%d", seq2))
	recGreen := httptest.NewRecorder()

	greenCtx, greenCancel := context.WithCancel(ctx)
	greenDone := make(chan struct{})
	go func() {
		defer close(greenDone)
		serverGreen.Handler().ServeHTTP(recGreen, reqGreen.WithContext(greenCtx))
	}()

	time.Sleep(50 * time.Millisecond)

	// Verify Green identity headers
	if slot := recGreen.Header().Get("X-Platform-Slot"); slot != "green" {
		t.Errorf("expected Green slot header 'green', got %q", slot)
	}
	if commit := recGreen.Header().Get("X-Release-Commit"); commit != "commit-green-2222" {
		t.Errorf("expected Green commit header 'commit-green-2222', got %q", commit)
	}

	// Publish live event 5 on Green
	seq5, err := store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_005", []byte(`{"amount":500000,"id":"txn_005"}`))
	if err != nil {
		t.Fatalf("failed to append event 5: %v", err)
	}
	hubGreen.Publish(eventhub.Event{
		Seq:         seq5,
		Epoch:       "ep1",
		EventType:   "bank.transaction.credit",
		AggregateID: "txn_005",
		Payload:     []byte(`{"amount":500000,"id":"txn_005"}`),
	})

	time.Sleep(50 * time.Millisecond)
	greenCancel()
	<-greenDone

	// Phase 5: Assert replay on Green received txn_003, txn_004, txn_005 without duplicates
	greenBody := recGreen.Body.String()
	if strings.Contains(greenBody, "txn_001") {
		t.Errorf("unexpected txn_001 in Green stream (should have been filtered by cursor)")
	}
	if strings.Contains(greenBody, "txn_002") {
		t.Errorf("unexpected txn_002 in Green stream (should have been filtered by cursor)")
	}
	if !strings.Contains(greenBody, "txn_003") {
		t.Errorf("expected replayed txn_003 in Green stream, got: %s", greenBody)
	}
	if !strings.Contains(greenBody, "txn_004") {
		t.Errorf("expected replayed txn_004 in Green stream, got: %s", greenBody)
	}
	if !strings.Contains(greenBody, "txn_005") {
		t.Errorf("expected live txn_005 in Green stream, got: %s", greenBody)
	}

	// Verify ordering: txn_003 appears before txn_004, which appears before txn_005
	idx3 := strings.Index(greenBody, "txn_003")
	idx4 := strings.Index(greenBody, "txn_004")
	idx5 := strings.Index(greenBody, "txn_005")
	if !(idx3 < idx4 && idx4 < idx5) {
		t.Errorf("expected ordered replay [txn_003 < txn_004 < txn_005], got indices %d, %d, %d", idx3, idx4, idx5)
	}
}
