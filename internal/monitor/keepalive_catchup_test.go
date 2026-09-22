package monitor

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type keepaliveMockClient struct {
	bootstrapCalls atomic.Int32
	historyCalls   atomic.Int32
}

func (m *keepaliveMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	m.bootstrapCalls.Add(1)
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *keepaliveMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	m.historyCalls.Add(1)
	body := `<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_A</td><td>12/09/2026 02:00:00</td><td>0</td><td>100,000</td><td>500,000</td><td>Transfer A</td></tr>
		<tr><td>TXN_B</td><td>12/09/2026 06:00:00</td><td>0</td><td>200,000</td><td>700,000</td><td>Transfer B</td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestKeepaliveCallsBootstrapAndNeverCallsHistory(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_mon_keepalive.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	connID := "conn_keepalive"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	mockClient := &keepaliveMockClient{}
	mon := New(store, mockClient, 5*time.Second, 15*time.Second)

	// Run pollKeepalive
	err = mon.pollKeepalive(ctx)
	if err != nil {
		t.Fatalf("pollKeepalive failed: %v", err)
	}

	if mockClient.bootstrapCalls.Load() != 1 {
		t.Errorf("expected Bootstrap calls = 1, got %d", mockClient.bootstrapCalls.Load())
	}
	if mockClient.historyCalls.Load() != 0 {
		t.Errorf("expected History calls = 0 during keepalive, got %d", mockClient.historyCalls.Load())
	}
}
