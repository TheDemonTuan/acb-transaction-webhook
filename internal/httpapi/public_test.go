package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestPublicAPI_SecurityAndDataIsolation(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_public.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	connID := "conn_public_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	// Insert transaction with sensitive balance
	bal := int64(987654321)
	items := []storage.BatchTransactionItem{
		{
			Number:        "TXN_PUBLIC_001",
			Credit:        50000,
			Debit:         0,
			Balance:       &bal,
			TransactionAt: "12/09/2026 10:00:00",
			EffectiveAt:   "12/09/2026",
			Description:   "Public donation test",
		},
	}
	res, err := store.IngestTransactionsBatch(ctx, connID, 1, "123***789", items, false)
	if err != nil {
		t.Fatalf("ingest transaction: %v", err)
	}
	if len(res.NewEvents) == 0 {
		t.Fatalf("expected emitted event with transaction ID")
	}
	seededTxID := res.NewEvents[0].TransactionID

	// Insert payment QR with internal metadata
	if _, err := store.SavePaymentQR(ctx, storage.PaymentQR{
		ConnectionID:     connID,
		AccountNumber:    "1234567890",
		AccountName:      "NGUYEN VIET TUAN",
		Bin:              "970416",
		BankName:         "ACB",
		ImagePath:        "internal/secret/path.png",
		ImageHash:        "secret_hash_123",
		ImageContentType: "image/png",
	}); err != nil {
		t.Fatalf("save payment qr: %v", err)
	}

	// Create server with production-like CF auth required
	cfg := config.Config{
		Production:         true,
		Timezone:           time.UTC,
		CloudflareIssuer:   "https://test.cloudflareaccess.com",
		CloudflareAudience: "cf-aud-prod",
		CloudflareJWKSURL:  "https://test.cloudflareaccess.com/cdn-cgi/access/certs",
		DatabasePath:       dbPath,
	}
	hub := eventhub.New()
	srv := New(cfg, store).WithEventHub(hub)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	t.Run("Regression: /api/v1/status without JWT is 401 Unauthorized", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/status", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /api/public/v1/transactions is 200 and leaks NO balance", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/public/v1/transactions", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("expected Cache-Control no-store, got %q", cc)
		}

		var page map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatalf("json decode: %v", err)
		}
		items, ok := page["items"].([]any)
		if !ok || len(items) == 0 {
			t.Fatalf("expected items array, got %v", page["items"])
		}
		firstItem := items[0].(map[string]any)
		if _, exists := firstItem["balance"]; exists {
			t.Errorf("SECURITY LEAK: balance field found in public transaction list: %v", firstItem["balance"])
		}
		rawJSON, _ := json.Marshal(page)
		if strings.Contains(string(rawJSON), "987654321") {
			t.Errorf("SECURITY LEAK: balance raw value 987654321 leaked in JSON: %s", string(rawJSON))
		}
		if firstItem["credit"] != float64(50000) {
			t.Errorf("expected credit 50000, got %v", firstItem["credit"])
		}
	})

	t.Run("GET /api/public/v1/transactions/{id} leaks NO balance", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/public/v1/transactions/%s", ts.URL, seededTxID), nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}

		var detail map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
			t.Fatalf("json decode: %v", err)
		}
		if _, exists := detail["balance"]; exists {
			t.Errorf("SECURITY LEAK: balance field found in public transaction detail: %v", detail["balance"])
		}
		rawJSON, _ := json.Marshal(detail)
		if strings.Contains(string(rawJSON), "987654321") {
			t.Errorf("SECURITY LEAK: balance raw value 987654321 leaked in JSON: %s", string(rawJSON))
		}
	})

	t.Run("GET /api/public/v1/payment-qr leaks NO internal metadata", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/public/v1/payment-qr", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}

		var qrResp map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&qrResp); err != nil {
			t.Fatalf("json decode: %v", err)
		}
		if qrResp["configured"] != true || qrResp["hasImage"] != true {
			t.Errorf("expected configured=true, hasImage=true, got %v", qrResp)
		}
		rawJSON, _ := json.Marshal(qrResp)
		rawStr := string(rawJSON)
		if strings.Contains(rawStr, "internal/secret/path.png") || strings.Contains(rawStr, "imagePath") {
			t.Errorf("SECURITY LEAK: imagePath leaked in public QR: %s", rawStr)
		}
		if strings.Contains(rawStr, "secret_hash_123") || strings.Contains(rawStr, "imageHash") {
			t.Errorf("SECURITY LEAK: imageHash leaked in public QR: %s", rawStr)
		}
		if strings.Contains(rawStr, "conn_public_test") || strings.Contains(rawStr, "connectionId") {
			t.Errorf("SECURITY LEAK: connectionId leaked in public QR: %s", rawStr)
		}
		qrObj, ok := qrResp["qr"].(map[string]any)
		if !ok {
			t.Fatalf("expected qr object, got %v", qrResp["qr"])
		}
		if qrObj["accountName"] != "NGUYEN VIET TUAN" || qrObj["accountNumber"] != "1234567890" || qrObj["bankName"] != "ACB" {
			t.Errorf("unexpected qr object content: %v", qrObj)
		}
	})

	t.Run("Public mutation attempts return 405 Method Not Allowed", func(t *testing.T) {
		methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
		for _, m := range methods {
			req, _ := http.NewRequest(m, ts.URL+"/api/public/v1/transactions", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("%s failed: %v", m, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s /api/public/v1/transactions: expected 405, got %d", m, resp.StatusCode)
			}
		}
	})

	t.Run("Public SSE stream filters out non-credit events", func(t *testing.T) {
		// Insert journal events directly: one audit, one credit, one connection
		nowTime := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO event_journal(epoch, event_type, aggregate_id, payload_json, created_at)
				VALUES
					('ep1', 'audit.created', 'aud_1', '{"action":"login"}', ?),
					('ep1', 'bank.transaction.credit', 'txn_1', '{"bank":"ACB","transactionId":"t1","transactionNumber":"N1","credit":"50000","debit":"0","currency":"VND","transactionDate":"2026-09-22T10:00:00Z","transactionDay":"2026-09-22","source":"REALTIME","description":"safe description","detectedAt":"2026-09-22T10:00:00Z","balance":"987654321","accountNumber":"123456789","sessionToken":"session-secret","cookie":"cookie-secret","rawForm":"raw-secret","dse_sessionId":"dse-secret"}', ?),
					('ep1', 'connection.changed', 'conn_1', '{"state":"MONITORING"}', ?)

		`, nowTime, nowTime, nowTime)
		if err != nil {
			t.Fatalf("insert journal: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/public/v1/events?lastEventId=ep1:0", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("SSE request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		reader := bufio.NewReader(resp.Body)
		var receivedEvents []string

		// Read first few SSE events with timeout
		readDone := make(chan struct{})
		streamBody := make(chan string, 1)
		go func() {
			defer close(readDone)
			var lines strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				lines.WriteString(line)
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "event: ") {
					evt := strings.TrimPrefix(line, "event: ")
					receivedEvents = append(receivedEvents, evt)
					if evt == "bank.transaction.credit" {
						streamBody <- lines.String()
						return
					}
				}
			}
		}()

		select {
		case <-readDone:
		case <-time.After(3 * time.Second):
		}

		for _, evt := range receivedEvents {
			if evt == "audit.created" || evt == "connection.changed" {
				t.Errorf("SECURITY LEAK: private event leaked in public SSE: %s", evt)
			}
		}
		foundCredit := false
		for _, evt := range receivedEvents {
			if evt == "bank.transaction.credit" {
				foundCredit = true
			}
		}
		if !foundCredit {
			t.Errorf("expected bank.transaction.credit in public SSE, received: %v", receivedEvents)
		}
		select {
		case body := <-streamBody:
			for _, secret := range []string{"987654321", "123456789", "session-secret", "cookie-secret", "raw-secret", "dse-secret"} {
				if strings.Contains(body, secret) {
					t.Errorf("SECURITY LEAK: sensitive credit field %q reached public SSE: %s", secret, body)
				}
			}
		default:
			t.Fatal("public SSE credit body was not captured")
		}

	})
}
