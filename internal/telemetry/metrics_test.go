package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRealtimeStreamTelemetryAndAlerts(t *testing.T) {
	reg := NewRegistry()
	reg.SetRealtimeStreamState(true, "stopped", "unauthorized")
	reg.RecordRealtimeReconnect()
	reg.RecordRealtimeDisconnect()
	reg.RecordRealtimeCoordinatorQueueFull()
	reg.RecordFallbackRecovery(2, true)
	reg.RecordFallbackRecovery(1, false)
	reg.RecordCommitToGateway(12 * time.Millisecond)
	reg.RecordCommitToBrowserSSE(18 * time.Millisecond)

	snap := reg.FullSnapshot()
	if snap.Realtime.StreamConnected != 0 || snap.Realtime.StreamReconnectTotal != 1 || snap.Realtime.StreamDisconnectTotal != 1 || snap.Realtime.CoordinatorQueueFullTotal != 1 {
		t.Fatalf("unexpected stream telemetry: %+v", snap.Realtime)
	}
	if snap.Realtime.FallbackRecoveryTotal != 3 || snap.Realtime.GapRepairTotal != 2 || snap.Realtime.RecentFallbackRecoveryReconciles != 2 {
		t.Fatalf("unexpected recovery telemetry: %+v", snap.Realtime)
	}
	if snap.Realtime.P95CommitToGatewayMs != 12 || snap.Realtime.P95CommitToBrowserSSEMs != 18 {
		t.Fatalf("unexpected commit latency: %+v", snap.Realtime)
	}
	alerts := EvaluateAlerts(snap)
	active := map[string]bool{}
	for _, alert := range alerts {
		active[alert.Name] = alert.Active
	}
	if !active[AlertRealtimeStreamDegraded] || !active[AlertRealtimeFallbackRecovery] {
		t.Fatalf("expected realtime alerts active: %+v", active)
	}
}

func TestMetricsP95Calculation(t *testing.T) {
	reg := NewRegistry()

	for i := 1; i <= 100; i++ {
		reg.RecordSSE(time.Duration(i*2) * time.Millisecond)
		reg.RecordWebhook(time.Duration(i*3) * time.Millisecond)
	}

	reg.SetConnectedClients(5)

	rep := reg.Report()
	if rep.ConnectedClients != 5 {
		t.Fatalf("expected 5 connected clients, got %d", rep.ConnectedClients)
	}
	if rep.P95SSEMs < 188 || rep.P95SSEMs > 192 {
		t.Fatalf("expected p95 sse ~190ms, got %f", rep.P95SSEMs)
	}
	if rep.P95WebhookMs < 282 || rep.P95WebhookMs > 288 {
		t.Fatalf("expected p95 webhook ~285ms, got %f", rep.P95WebhookMs)
	}
}

func TestNotificationDeliveryMetrics(t *testing.T) {
	reg := NewRegistry()
	reg.RecordWebhook(5*time.Millisecond, true)
	reg.RecordWebhook(10*time.Millisecond, false)
	reg.RecordNotification("WEBHOOK", 5*time.Millisecond, true)
	reg.RecordNotification("WEBHOOK", 10*time.Millisecond, false)
	reg.RecordNotification("BARK", 15*time.Millisecond, true)
	reg.SetNotificationBacklog(3, 1, false, map[string]ProviderSnapshot{
		"WEBHOOK": {Pending: 2, DeadLetter: 1},
		"BARK":    {Pending: 1},
	})

	report := reg.Report()
	if report.TotalWebhooksSent != 1 || report.Notifications["WEBHOOK"].Success != 1 || report.Notifications["WEBHOOK"].Failure != 1 {
		t.Fatalf("unexpected outbound delivery counters: %+v", report)
	}
	snapshot := reg.FullSnapshot()
	if snapshot.Realtime.TotalWebhooksSent != report.TotalWebhooksSent || snapshot.Notifications.TotalDelivered != 2 || snapshot.Notifications.TotalFailed != 1 {
		t.Fatalf("unexpected notification snapshot: %+v", snapshot)
	}
	if snapshot.Notifications.TotalPending != 3 || snapshot.Notifications.TotalDeadLetter != 1 || snapshot.Notifications.ByProvider["BARK"].Success != 1 {
		t.Fatalf("backlog or provider metrics lost: %+v", snapshot.Notifications)
	}
}

func TestTelemetrySnapshotRuntimeContract(t *testing.T) {
	reg := NewRegistry()
	for _, value := range []any{reg.Report(), reg.FullSnapshot()} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(encoded, &payload); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"scheduler", "historyJobs", "authLifecycle"} {
			if _, exists := payload[key]; exists {
				t.Fatalf("retired runtime telemetry %q remains in payload", key)
			}
		}
		realtime := payload
		if nested, ok := payload["realtime"].(map[string]any); ok {
			realtime = nested
		}
		for _, key := range []string{"lastAcbPollAt", "lastAcbPollAgeSeconds", "lastAcbPollDurationMs", "lastAcbPollStatus", "catchUpDay", "circuitBreakerOpen", "p95IngestMs", "totalIngested"} {
			if _, exists := realtime[key]; exists {
				t.Fatalf("retired bank telemetry %q remains in realtime payload", key)
			}
		}
		for _, key := range []string{"connectedClients", "p95SseMs", "p95WebhookMs", "totalWebhooksSent"} {
			if _, exists := realtime[key]; !exists {
				t.Fatalf("retained realtime telemetry %q missing", key)
			}
		}
	}
}
