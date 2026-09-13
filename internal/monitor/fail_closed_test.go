package monitor

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type failClosedMockClient struct {
	bootstrapCalls atomic.Int32
	historyCalls   atomic.Int32
}

func (m *failClosedMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	m.bootstrapCalls.Add(1)
	return acb.Response{Kind: acb.AccountDetailPage, StatusCode: 200}, nil
}

func (m *failClosedMockClient) History(ctx context.Context, action string, fields map[string]string) (acb.Response, error) {
	m.historyCalls.Add(1)
	return acb.Response{Kind: acb.HistoryPage, StatusCode: 200}, nil
}

func TestPollOnce_FailClosed_OnActiveAuthLookupError(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fail_closed.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatalf("configure connection: %v", err)
	}

	// Set connection to MONITORING state
	_, err = store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING' WHERE id = ?", conn.ID)
	if err != nil {
		t.Fatalf("update connection state: %v", err)
	}

	mockClient := &failClosedMockClient{}
	m := New(store, mockClient, 3*time.Second, 10*time.Second)

	// Simulate database failure for auth attempts lookup by dropping the table
	_, err = store.DB().ExecContext(ctx, "DROP TABLE auth_attempts")
	if err != nil {
		t.Fatalf("drop auth_attempts: %v", err)
	}

	err = m.PollOnce(ctx)
	if err == nil {
		t.Fatal("expected PollOnce to fail closed when HasActiveAuthAttempt returns an error, got nil")
	}

	if !strings.Contains(err.Error(), "check active auth attempt") {
		t.Fatalf("expected error mentioning 'check active auth attempt', got: %v", err)
	}

	if mockClient.bootstrapCalls.Load() != 0 {
		t.Fatalf("expected 0 Bootstrap calls on DB error, got %d", mockClient.bootstrapCalls.Load())
	}
	if mockClient.historyCalls.Load() != 0 {
		t.Fatalf("expected 0 History calls on DB error, got %d", mockClient.historyCalls.Load())
	}
}
