package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

	// Setup physical test image in qr directory
	qrDir := filepath.Join(dbDir, "qr")
	if err := os.MkdirAll(qrDir, 0o750); err != nil {
		t.Fatalf("mkdir qr: %v", err)
	}
	qrFile := filepath.Join(qrDir, "qr_test.png")
	qrBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15c4\x00\x00\x00\rIDATx\x9cc`\x00\x00\x00\x02\x00\x01H\xaf\xa4q\x00\x00\x00\x00IEND\xaeB`\x82")
	if err := os.WriteFile(qrFile, qrBytes, 0o644); err != nil {
		t.Fatalf("write qr file: %v", err)
	}
	qrHash := storage.HashBytes(qrBytes)

	// Insert payment QR with internal metadata
	if _, err := store.SavePaymentQR(ctx, storage.PaymentQR{
		ConnectionID:     connID,
		AccountNumber:    "1234567890",
		AccountName:      "NGUYEN VIET TUAN",
		Bin:              "970416",
		BankName:         "ACB",
		ImagePath:        qrFile,
		ImageHash:        qrHash,
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
		if strings.Contains(rawStr, qrFile) || strings.Contains(rawStr, "imagePath") {
			t.Errorf("SECURITY LEAK: imagePath leaked in public QR: %s", rawStr)
		}
		if strings.Contains(rawStr, qrHash) || strings.Contains(rawStr, "imageHash") {
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

	t.Run("GET /api/public/v1/payment-qr/image serves valid image content and headers", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/public/v1/payment-qr/image", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("expected Content-Type image/png, got %q", ct)
		}
		expectedETag := fmt.Sprintf(`"%s"`, qrHash)
		if etag := resp.Header.Get("ETag"); etag != expectedETag {
			t.Errorf("expected ETag %q, got %q", expectedETag, etag)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=3600" {
			t.Errorf("expected Cache-Control private, max-age=3600, got %q", cc)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if len(body) == 0 {
			t.Fatal("expected non-empty image body")
		}
		if !bytes.Equal(body, qrBytes) {
			t.Errorf("body bytes mismatch with stored PNG: expected %d bytes, got %d bytes", len(qrBytes), len(body))
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

	t.Run("Public SSE stream delivers credit and activation events while filtering private events", func(t *testing.T) {
		// Insert journal events directly: one audit, one credit, one activation, one connection
		nowTime := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO event_journal(epoch, event_type, aggregate_id, payload_json, created_at)
			VALUES
				('ep1', 'audit.created', 'aud_1', '{"action":"login"}', ?),
				('ep1', 'bank.transaction.credit', 'txn_1', '{"credit":"50000","transactionId":"t1"}', ?),
				('ep1', 'payment.activated', 'act_1', '{"identifier":"act_1","phase":"HOT"}', ?),
				('ep1', 'connection.changed', 'conn_1', '{"state":"MONITORING"}', ?)
		`, nowTime, nowTime, nowTime, nowTime)
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
		go func() {
			defer close(readDone)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "event: ") {
					evt := strings.TrimPrefix(line, "event: ")
					receivedEvents = append(receivedEvents, evt)
					if len(receivedEvents) >= 3 {
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
		foundActivation := false
		for _, evt := range receivedEvents {
			if evt == "bank.transaction.credit" {
				foundCredit = true
			}
			if evt == "payment.activated" {
				foundActivation = true
			}
		}
		if !foundCredit {
			t.Errorf("expected bank.transaction.credit in public SSE, received: %v", receivedEvents)
		}
		if !foundActivation {
			t.Errorf("expected payment.activated in public SSE, received: %v", receivedEvents)
		}
	})
}
