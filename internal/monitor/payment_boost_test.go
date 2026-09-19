package monitor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestPaymentBoost_PhasesAndExpiration(t *testing.T) {
	m := &Monitor{
		now:     time.Now,
		boostCh: make(chan struct{}, 1),
	}

	baseTime := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	currentTime := baseTime
	m.now = func() time.Time { return currentTime }

	// 1. Initially inactive
	status := m.PaymentBoostStatus()
	if status.Active {
		t.Fatalf("expected inactive boost initially, got %+v", status)
	}

	// 2. Start boost with 100,000 VND
	status = m.StartPaymentBoost(100000)
	if !status.Active || status.AmountVnd != 100000 || status.Phase != 1 || status.MinSeconds != 1 || status.MaxSeconds != 3 || status.SessionID == "" {
		t.Fatalf("unexpected initial boost status: %+v", status)
	}
	if status.ExpiresIn != 180 {
		t.Fatalf("expected expiresIn 180s, got %d", status.ExpiresIn)
	}

	// Verify wake channel received signal
	select {
	case <-m.boostCh:
		// expected
	default:
		t.Fatal("expected wake signal in boostCh")
	}

	// 3. Advance to 59s -> still Phase 1 (1-3s)
	currentTime = baseTime.Add(59 * time.Second)
	status = m.PaymentBoostStatus()
	if !status.Active || status.Phase != 1 || status.MinSeconds != 1 || status.MaxSeconds != 3 {
		t.Fatalf("expected Phase 1 at 59s, got %+v", status)
	}

	// 4. Advance to 60s -> Phase 2 (3-6s)
	currentTime = baseTime.Add(60 * time.Second)
	status = m.PaymentBoostStatus()
	if !status.Active || status.Phase != 2 || status.MinSeconds != 3 || status.MaxSeconds != 6 {
		t.Fatalf("expected Phase 2 at 60s, got %+v", status)
	}

	// 5. Advance to 120s -> Phase 3 (6-10s)
	currentTime = baseTime.Add(120 * time.Second)
	status = m.PaymentBoostStatus()
	if !status.Active || status.Phase != 3 || status.MinSeconds != 6 || status.MaxSeconds != 10 {
		t.Fatalf("expected Phase 3 at 120s, got %+v", status)
	}

	// 6. Advance to 181s -> Expired (inactive)
	currentTime = baseTime.Add(181 * time.Second)
	status = m.PaymentBoostStatus()
	if status.Active {
		t.Fatalf("expected boost to expire after 180s, got %+v", status)
	}
}

func TestPaymentBoost_AutoStopOnMatchingCredit(t *testing.T) {
	m := &Monitor{
		now:     time.Now,
		boostCh: make(chan struct{}, 1),
	}

	boostStart := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return boostStart }

	// Start boost expecting 250,000 VND
	m.StartPaymentBoost(250000)

	// Case A: Debit event -> does not stop boost
	m.checkAndStopBoost([]storage.EventNotification{
		{
			EventType: "bank.transaction.debit",
			Payload:   []byte(`{"credit":"0","debit":"250000"}`),
		},
	})
	if !m.PaymentBoostStatus().Active {
		t.Fatal("expected boost to remain active on debit event")
	}

	// Case B: Credit with different amount -> does not stop boost
	m.checkAndStopBoost([]storage.EventNotification{
		{
			EventType: "bank.transaction.credit",
			Payload:   []byte(`{"credit":"100000","detectedAt":"2026-09-19T12:00:05Z"}`),
		},
	})
	if !m.PaymentBoostStatus().Active {
		t.Fatal("expected boost to remain active on mismatched credit amount")
	}

	// Case C: Credit with matching amount but old timestamp (>2s before boost start) -> does not stop boost
	m.checkAndStopBoost([]storage.EventNotification{
		{
			EventType: "bank.transaction.credit",
			Payload:   []byte(`{"credit":"250000","detectedAt":"2026-09-19T11:59:50Z"}`),
		},
	})
	if !m.PaymentBoostStatus().Active {
		t.Fatal("expected boost to remain active on old credit event")
	}

	// Case D: Credit with matching amount and fresh timestamp -> STOPS boost!
	matchingPayload, _ := json.Marshal(map[string]any{
		"credit":     "250000",
		"detectedAt": boostStart.Add(5 * time.Second).Format(time.RFC3339),
	})
	m.checkAndStopBoost([]storage.EventNotification{
		{
			EventID:   "evt_match_1",
			EventType: "bank.transaction.credit",
			Payload:   matchingPayload,
		},
	})
	if m.PaymentBoostStatus().Active {
		t.Fatal("expected boost to stop on matching credit transaction")
	}
}

func TestPaymentBoost_AutoStopAnyAmount(t *testing.T) {
	m := &Monitor{
		now:     time.Now,
		boostCh: make(chan struct{}, 1),
	}

	boostStart := time.Date(2026, 9, 19, 14, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return boostStart }

	// Start boost with 0 (any amount)
	m.StartPaymentBoost(0)
	if !m.PaymentBoostStatus().Active {
		t.Fatal("expected active boost")
	}

	// Fresh credit of 50,000 VND -> stops boost
	matchingPayload, _ := json.Marshal(map[string]any{
		"credit":     "50000",
		"detectedAt": boostStart.Add(2 * time.Second).Format(time.RFC3339),
	})
	m.checkAndStopBoost([]storage.EventNotification{
		{
			EventID:   "evt_any_1",
			EventType: "bank.transaction.credit",
			Payload:   matchingPayload,
		},
	})
	if m.PaymentBoostStatus().Active {
		t.Fatal("expected boost with ExpectedAmount=0 to stop on any fresh credit")
	}
}

func TestPaymentBoost_ManualStop(t *testing.T) {
	m := &Monitor{
		now:     time.Now,
		boostCh: make(chan struct{}, 1),
	}
	m.StartPaymentBoost(50000)
	if !m.PaymentBoostStatus().Active {
		t.Fatal("expected active boost")
	}
	m.StopPaymentBoost("")
	if m.PaymentBoostStatus().Active {
		t.Fatal("expected inactive boost after StopPaymentBoost")
	}
}

func TestPaymentBoost_SessionScopedStop(t *testing.T) {
	m := &Monitor{
		now:     time.Now,
		boostCh: make(chan struct{}, 1),
	}
	st1 := m.StartPaymentBoost(50000)
	if !st1.Active || st1.SessionID == "" {
		t.Fatalf("expected active boost with session ID, got %+v", st1)
	}

	// Another session ID tries to stop -> ignored
	m.StopPaymentBoost("wrong-session-id")
	if !m.PaymentBoostStatus().Active {
		t.Fatal("expected boost to remain active when stopped with mismatched session ID")
	}

	// Correct session ID stops boost
	m.StopPaymentBoost(st1.SessionID)
	if m.PaymentBoostStatus().Active {
		t.Fatal("expected boost to stop when stopped with matching session ID")
	}

	// Unconditional stop (empty session ID) also stops boost
	st2 := m.StartPaymentBoost(60000)
	if !st2.Active {
		t.Fatal("expected active boost")
	}
	m.StopPaymentBoost("")
	if m.PaymentBoostStatus().Active {
		t.Fatal("expected boost to stop with empty session ID")
	}
}
