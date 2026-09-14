package telemetry

import (
	"testing"
	"time"
)

func TestTelemetry_PrivacyAndCardinalityControls(t *testing.T) {
	reg := NewRegistry()
	reg.SetSchedulerQueue(map[string]int{"REALTIME_POLL": 1, "FILTER_HISTORY": 2}, 3)
	reg.SetHistoryJobs(map[string]int{"QUEUED": 2, "RUNNING": 1, "COMPLETED": 10}, 15*time.Second, 0, 50, 1200)
	reg.SetAuthLifecycle(false, 0, false, "READY", 3, map[string]int{"COMPLETED": 3})
	reg.SetNotificationBacklog(2, 0, false, map[string]ProviderSnapshot{
		"WEBHOOK": {Pending: 1, Success: 100},
		"BARK":    {Pending: 1, Success: 50},
	})
	reg.SetWorkerSingleton("worker", "READY", true, "NONE", time.Now().UTC(), 60*time.Second)
	reg.SetDeployment("blue", "release-commit-sha", "gateway", "8", "SUCCESS", time.Now().UTC())
	reg.SetBackup("gateway-20260914120000.db.age", time.Now().UTC().Add(-1*time.Hour))
	reg.SetRestoreDrill(time.Now().UTC().Add(-2*24*time.Hour), true)

	snap := reg.FullSnapshot()

	t.Run("valid snapshot contains no secrets", func(t *testing.T) {
		if err := ValidateNoSecrets(snap); err != nil {
			t.Fatalf("expected clean snapshot, got error: %v", err)
		}
	})

	t.Run("detects raw Bearer token", func(t *testing.T) {
		leaky := map[string]any{
			"snapshot": snap,
			"leak":     "Bearer my-internal-secret-token-12345",
		}
		if err := ValidateNoSecrets(leaky); err == nil {
			t.Fatal("expected ValidateNoSecrets to catch raw Bearer token, got nil")
		}
	})

	t.Run("detects JWT token", func(t *testing.T) {
		leaky := map[string]any{
			"jwt": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgN_p_placeholder",
		}
		if err := ValidateNoSecrets(leaky); err == nil {
			t.Fatal("expected ValidateNoSecrets to catch JWT, got nil")
		}
	})

	t.Run("detects age secret key", func(t *testing.T) {
		leaky := map[string]any{
			"key": "AGE-SECRET-KEY-1XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX",
		}
		if err := ValidateNoSecrets(leaky); err == nil {
			t.Fatal("expected ValidateNoSecrets to catch age secret key, got nil")
		}
	})

	t.Run("detects raw ACB HTML table markup", func(t *testing.T) {
		leaky := map[string]any{
			"rawHTML": "<table class='acb-grid'><tr><td>Transaction</td></tr></table>",
		}
		if err := ValidateNoSecrets(leaky); err == nil {
			t.Fatal("expected ValidateNoSecrets to catch raw ACB HTML, got nil")
		}
	})

	t.Run("detects high-cardinality UUID key in map", func(t *testing.T) {
		leaky := map[string]any{
			"550e8400-e29b-41d4-a716-446655440000": 42,
		}
		if err := ValidateNoSecrets(leaky); err == nil {
			t.Fatal("expected ValidateNoSecrets to reject UUID map key, got nil")
		}
	})
}
