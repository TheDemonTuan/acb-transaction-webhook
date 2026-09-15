package telemetry

import (
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
		reg.RecordIngest(time.Duration(i) * time.Millisecond)
		reg.RecordSSE(time.Duration(i*2) * time.Millisecond)
		reg.RecordWebhook(time.Duration(i*3) * time.Millisecond)
	}

	reg.SetConnectedClients(5)
	reg.SetCircuitBreaker(false)
	reg.SetLastACBPollAt(time.Now())

	rep := reg.Report()
	if rep.ConnectedClients != 5 {
		t.Fatalf("expected 5 connected clients, got %d", rep.ConnectedClients)
	}
	if rep.P95IngestMs < 94 || rep.P95IngestMs > 96 {
		t.Fatalf("expected p95 ingest ~95ms, got %f", rep.P95IngestMs)
	}
	if rep.P95SSEMs < 188 || rep.P95SSEMs > 192 {
		t.Fatalf("expected p95 sse ~190ms, got %f", rep.P95SSEMs)
	}
	if rep.P95WebhookMs < 282 || rep.P95WebhookMs > 288 {
		t.Fatalf("expected p95 webhook ~285ms, got %f", rep.P95WebhookMs)
	}
}
